package rpc

import (
	"encoding/json"
	"fmt"
)

// State event types: the event types that change operation state. The client
// delivers only these on Events(). Every other event type is activity only: it
// updates LastActivity and is not delivered. Activity-only types include
// message_start, the per-token message_update, tool execution progress, and
// types that later Pi versions add.
const (
	EventAgentStart             = "agent_start"
	EventAgentEnd               = "agent_end"
	EventAgentSettled           = "agent_settled"
	EventTurnStart              = "turn_start"
	EventTurnEnd                = "turn_end"
	EventMessageEnd             = "message_end"
	EventCompactionStart        = "compaction_start"
	EventCompactionEnd          = "compaction_end"
	EventQueueUpdate            = "queue_update"
	EventAutoRetryStart         = "auto_retry_start"
	EventAutoRetryEnd           = "auto_retry_end"
	EventSummarizationScheduled = "summarization_retry_scheduled"
)

// isStateEvent reports whether eventType is a state event type.
func isStateEvent(eventType string) bool {
	switch eventType {
	case EventAgentStart, EventAgentEnd, EventAgentSettled,
		EventTurnStart, EventTurnEnd, EventMessageEnd,
		EventCompactionStart, EventCompactionEnd, EventQueueUpdate,
		EventAutoRetryStart, EventAutoRetryEnd, EventSummarizationScheduled:
		return true
	}
	return false
}

// Extension UI record type names.
const (
	UIRequestType  = "extension_ui_request"
	UIResponseType = "extension_ui_response"
)

// Disposition is the outcome of a prompt or steer command.
type Disposition string

// Disposition values reported by Pi in prompt/steer response data.
const (
	DispositionStarted Disposition = "started"
	DispositionQueued  Disposition = "queued"
	DispositionHandled Disposition = "handled"
)

// Command is a send-side command record. Send injects the request ID; the
// builders leave it empty.
type Command struct {
	ID                string `json:"id,omitempty"`
	Type              string `json:"type"`
	Message           string `json:"message,omitempty"`
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
}

// PromptCommand builds a prompt command with the given streaming behavior.
func PromptCommand(message, streamingBehavior string) Command {
	return Command{Type: "prompt", Message: message, StreamingBehavior: streamingBehavior}
}

// SteerCommand builds a bare steering command.
func SteerCommand(message string) Command {
	return Command{Type: "steer", Message: message}
}

// AbortCommand builds an abort command.
func AbortCommand() Command { return Command{Type: "abort"} }

// ClearQueueCommand builds a clear_queue command.
func ClearQueueCommand() Command { return Command{Type: "clear_queue"} }

// NewSessionCommand builds a new_session command.
func NewSessionCommand() Command { return Command{Type: "new_session"} }

// GetStateCommand builds a get_state command.
func GetStateCommand() Command { return Command{Type: "get_state"} }

// GetCommandsCommand builds a get_commands command.
func GetCommandsCommand() Command { return Command{Type: "get_commands"} }

// GetLastAssistantTextCommand builds a get_last_assistant_text command.
func GetLastAssistantTextCommand() Command { return Command{Type: "get_last_assistant_text"} }

// Response is the command response envelope. A response without an ID is a
// parse error from Pi rejecting a malformed command.
type Response struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Decode decodes the response data field into v.
func (r Response) Decode(v any) error {
	if len(r.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Data, v); err != nil {
		return fmt.Errorf("decode %s response data: %w", r.Command, err)
	}
	return nil
}

// PromptData is the data of a prompt or steer response.
type PromptData struct {
	Disposition Disposition `json:"disposition"`
}

// QueueContents is the data of a clear_queue response.
type QueueContents struct {
	Steering []string `json:"steering"`
	FollowUp []string `json:"followUp"`
}

// NewSessionData is the data of a new_session response.
type NewSessionData struct {
	Cancelled bool `json:"cancelled"`
}

// State is the data of a get_state response.
type State struct {
	SessionID           string `json:"sessionId"`
	SessionFile         string `json:"sessionFile"`
	IsStreaming         bool   `json:"isStreaming"`
	IsCompacting        bool   `json:"isCompacting"`
	PendingMessageCount int    `json:"pendingMessageCount"`
	MessageCount        int    `json:"messageCount"`
	Model               *Model `json:"model,omitempty"`
}

// Model is the model object Pi reports in get_state when a model is selected.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	API      string `json:"api,omitempty"`
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"baseUrl,omitempty"`
}

// CommandInfo is one entry of a get_commands response.
type CommandInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source"`
	SourceInfo  json.RawMessage `json:"sourceInfo,omitempty"`
}

// CommandsData is the data of a get_commands response.
type CommandsData struct {
	Commands []CommandInfo `json:"commands"`
}

// LastAssistantTextData is the data of a get_last_assistant_text response.
// Text is a pointer so JSON null (no assistant text) is distinguishable from
// an empty string.
type LastAssistantTextData struct {
	Text *string `json:"text"`
}

// Event is one session event record. Only the fields the coordinator consumes
// are decoded; content-bearing fields (usage, deltas, tool arguments) are
// intentionally dropped so the component never retains prompt or tool payloads.
type Event struct {
	Type         string          `json:"type"`
	WillRetry    bool            `json:"willRetry,omitempty"`
	Messages     []Message       `json:"messages,omitempty"`
	Message      *Message        `json:"message,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	FinalError   string          `json:"finalError,omitempty"`
	Steering     []string        `json:"steering,omitempty"`
	FollowUp     []string        `json:"followUp,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
}

// Message is a session message, decoded only for the fields the coordinator
// consumes (role, stop reason, and failure text). Content is dropped; the
// REPL never prints from the event stream.
type Message struct {
	Role         string `json:"role"`
	StopReason   string `json:"stopReason,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// UIRequest is an extension_ui_request record from Pi. Dialog methods expect
// an extension_ui_response; fire-and-forget methods do not.
type UIRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
}

// UIResponse is an extension_ui_response record sent to Pi.
type UIResponse struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled"`
}

// CancelledUIResponse builds the only extension UI answer Trygalle sends:
// every dialog is cancelled immediately.
func CancelledUIResponse(id string) UIResponse {
	return UIResponse{Type: UIResponseType, ID: id, Cancelled: true}
}
