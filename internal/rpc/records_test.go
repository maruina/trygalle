package rpc

import (
	"encoding/json"
	"testing"
)

func TestCommandBuildersMarshal(t *testing.T) {
	cases := []struct {
		name string
		cmd  Command
		want string
	}{
		{
			name: "prompt with streaming behavior",
			cmd:  PromptCommand("Hello", "steer"),
			want: `{"type":"prompt","message":"Hello","streamingBehavior":"steer"}`,
		},
		{
			name: "steer",
			cmd:  SteerCommand("Stop and do this instead"),
			want: `{"type":"steer","message":"Stop and do this instead"}`,
		},
		{
			name: "abort",
			cmd:  AbortCommand(),
			want: `{"type":"abort"}`,
		},
		{
			name: "clear_queue",
			cmd:  ClearQueueCommand(),
			want: `{"type":"clear_queue"}`,
		},
		{
			name: "new_session",
			cmd:  NewSessionCommand(),
			want: `{"type":"new_session"}`,
		},
		{
			name: "get_state",
			cmd:  GetStateCommand(),
			want: `{"type":"get_state"}`,
		},
		{
			name: "get_commands",
			cmd:  GetCommandsCommand(),
			want: `{"type":"get_commands"}`,
		},
		{
			name: "get_last_assistant_text",
			cmd:  GetLastAssistantTextCommand(),
			want: `{"type":"get_last_assistant_text"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.cmd)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("marshaled = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestResponseEnvelopeDecode(t *testing.T) {
	t.Run("success with data", func(t *testing.T) {
		golden := `{"id":"req-1","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`
		var r Response
		if err := json.Unmarshal([]byte(golden), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.ID != "req-1" || r.Type != "response" || r.Command != "prompt" || !r.Success {
			t.Errorf("response fields wrong: %+v", r)
		}
		var d PromptData
		if err := r.Decode(&d); err != nil {
			t.Fatalf("decode data: %v", err)
		}
		if d.Disposition != DispositionStarted {
			t.Errorf("disposition = %q, want %q", d.Disposition, DispositionStarted)
		}
	})

	t.Run("error response", func(t *testing.T) {
		golden := `{"id":"req-3","type":"response","command":"set_model","success":false,"error":"Model not found: invalid/model"}`
		var r Response
		if err := json.Unmarshal([]byte(golden), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.Success || r.Error != "Model not found: invalid/model" {
			t.Errorf("error response wrong: %+v", r)
		}
	})

	t.Run("parse response without id", func(t *testing.T) {
		golden := `{"type":"response","command":"parse","success":false,"error":"Failed to parse command: Unexpected token..."}`
		var r Response
		if err := json.Unmarshal([]byte(golden), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.ID != "" || r.Command != "parse" || r.Success {
			t.Errorf("parse response wrong: %+v", r)
		}
	})
}

func TestPromptDataDecode(t *testing.T) {
	for _, want := range []Disposition{DispositionStarted, DispositionQueued, DispositionHandled} {
		golden := `{"disposition":"` + string(want) + `"}`
		var data PromptData
		if err := json.Unmarshal([]byte(golden), &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data.Disposition != want {
			t.Errorf("disposition = %q, want %q", data.Disposition, want)
		}
	}
}

func TestQueueContentsDecode(t *testing.T) {
	golden := `{"steering":["Change direction"],"followUp":["Summarize when finished"]}`
	var data QueueContents
	if err := json.Unmarshal([]byte(golden), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(data.Steering) != 1 || data.Steering[0] != "Change direction" {
		t.Errorf("steering = %v", data.Steering)
	}
	if len(data.FollowUp) != 1 || data.FollowUp[0] != "Summarize when finished" {
		t.Errorf("followUp = %v", data.FollowUp)
	}
}

func TestNewSessionDataDecode(t *testing.T) {
	for _, want := range []bool{false, true} {
		golden := `{"cancelled":false}`
		if want {
			golden = `{"cancelled":true}`
		}
		var data NewSessionData
		if err := json.Unmarshal([]byte(golden), &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data.Cancelled != want {
			t.Errorf("cancelled = %v, want %v", data.Cancelled, want)
		}
	}
}

func TestStateDecodeFull(t *testing.T) {
	golden := `{"model":{"id":"mock-model","name":"Mock","api":"openai-completions","provider":"mock","baseUrl":"http://127.0.0.1:1"},"thinkingLevel":"medium","isStreaming":true,"isCompacting":false,"sessionFile":"/tmp/sessions/s.jsonl","sessionId":"abc123","messageCount":5,"pendingMessageCount":2}`
	var data State
	if err := json.Unmarshal([]byte(golden), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.SessionID != "abc123" || data.SessionFile != "/tmp/sessions/s.jsonl" {
		t.Errorf("session fields wrong: %+v", data)
	}
	if !data.IsStreaming || data.IsCompacting {
		t.Errorf("streaming flags wrong: %+v", data)
	}
	if data.PendingMessageCount != 2 || data.MessageCount != 5 {
		t.Errorf("counts wrong: %+v", data)
	}
	if data.Model == nil || data.Model.ID != "mock-model" || data.Model.Provider != "mock" {
		t.Errorf("model wrong: %+v", data.Model)
	}
}

func TestStateDecodeWithoutModel(t *testing.T) {
	golden := `{"isStreaming":false,"isCompacting":false,"sessionId":"abc123","messageCount":0,"pendingMessageCount":0}`
	var data State
	if err := json.Unmarshal([]byte(golden), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.Model != nil {
		t.Errorf("model = %+v, want nil", data.Model)
	}
}

func TestCommandsDataDecode(t *testing.T) {
	golden := `{"commands":[{"name":"fix-tests","description":"Fix failing tests","source":"prompt","sourceInfo":{"path":"/p/fix-tests.md","source":"local","scope":"project","origin":"top-level"}}]}`
	var data CommandsData
	if err := json.Unmarshal([]byte(golden), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(data.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(data.Commands))
	}
	command := data.Commands[0]
	if command.Name != "fix-tests" || command.Description != "Fix failing tests" || command.Source != "prompt" {
		t.Errorf("command info wrong: %+v", command)
	}
	if len(command.SourceInfo) == 0 {
		t.Error("sourceInfo not preserved")
	}
}

func TestLastAssistantTextNull(t *testing.T) {
	var data LastAssistantTextData
	if err := json.Unmarshal([]byte(`{"text":null}`), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.Text != nil {
		t.Errorf("text = %v, want nil", *data.Text)
	}
}

func TestLastAssistantTextEmpty(t *testing.T) {
	var data LastAssistantTextData
	if err := json.Unmarshal([]byte(`{"text":""}`), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.Text == nil || *data.Text != "" {
		t.Errorf("text = %v, want empty string", data.Text)
	}
}

func TestLastAssistantTextOmitted(t *testing.T) {
	// Live Pi 1.0.1 returns data {} when no assistant text exists.
	var data LastAssistantTextData
	if err := json.Unmarshal([]byte(`{}`), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data.Text != nil {
		t.Errorf("text = %v, want nil for omitted field", *data.Text)
	}
}

func TestAgentStartEventDecode(t *testing.T) {
	var event Event
	if err := json.Unmarshal([]byte(`{"type":"agent_start"}`), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventAgentStart {
		t.Errorf("type = %q", event.Type)
	}
}

func TestAgentEndEventDecode(t *testing.T) {
	golden := `{"type":"agent_end","messages":[{"role":"assistant","content":[],"stopReason":"error","errorMessage":"boom","timestamp":1}],"willRetry":false}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventAgentEnd || event.WillRetry {
		t.Errorf("agent_end fields wrong: %+v", event)
	}
	if len(event.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(event.Messages))
	}
	message := event.Messages[0]
	if message.Role != "assistant" || message.StopReason != "error" || message.ErrorMessage != "boom" {
		t.Errorf("message wrong: %+v", message)
	}
}

func TestAgentSettledEventDecode(t *testing.T) {
	var event Event
	if err := json.Unmarshal([]byte(`{"type":"agent_settled"}`), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventAgentSettled {
		t.Errorf("type = %q", event.Type)
	}
}

func TestMessageEndEventDecode(t *testing.T) {
	golden := `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Provider error","timestamp":1}}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventMessageEnd || event.Message == nil {
		t.Fatalf("message_end wrong: %+v", event)
	}
	if event.Message.StopReason != "error" || event.Message.ErrorMessage != "Provider error" {
		t.Errorf("message wrong: %+v", event.Message)
	}
}

func TestCompactionStartEventDecode(t *testing.T) {
	var event Event
	if err := json.Unmarshal([]byte(`{"type":"compaction_start","reason":"threshold"}`), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventCompactionStart || event.Reason != "threshold" {
		t.Errorf("compaction_start wrong: %+v", event)
	}
}

func TestCompactionEndEventDecode(t *testing.T) {
	golden := `{"type":"compaction_end","reason":"threshold","result":{"summary":"..."},"aborted":false,"willRetry":false}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventCompactionEnd || event.Reason != "threshold" || len(event.Result) == 0 {
		t.Errorf("compaction_end wrong: %+v", event)
	}
}

func TestQueueUpdateEventDecode(t *testing.T) {
	golden := `{"type":"queue_update","steering":["one"],"followUp":["two","three"]}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(event.Steering) != 1 || event.Steering[0] != "one" {
		t.Errorf("steering wrong: %v", event.Steering)
	}
	if len(event.FollowUp) != 2 || event.FollowUp[1] != "three" {
		t.Errorf("followUp wrong: %v", event.FollowUp)
	}
}

func TestAutoRetryStartEventDecode(t *testing.T) {
	golden := `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"529 overloaded"}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != EventAutoRetryStart || event.ErrorMessage != "529 overloaded" {
		t.Errorf("auto_retry_start wrong: %+v", event)
	}
}

func TestUnrelatedEventFieldsDropped(t *testing.T) {
	// message_update carries usage and assistantMessageEvent; the component
	// must not retain them.
	golden := `{"type":"message_update","usage":{"input":100,"output":1,"totalTokens":101},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello "}}`
	var event Event
	if err := json.Unmarshal([]byte(golden), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != "message_update" {
		t.Errorf("type = %q", event.Type)
	}
}

func TestUIRequestDecode(t *testing.T) {
	for _, golden := range []string{
		`{"type":"extension_ui_request","id":"uuid-1","method":"select","title":"Allow dangerous command?","options":["Allow","Block"],"timeout":10000}`,
		`{"type":"extension_ui_request","id":"uuid-2","method":"notify","title":"Done","message":"finished"}`,
	} {
		var u UIRequest
		if err := json.Unmarshal([]byte(golden), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Type != UIRequestType || u.ID == "" || u.Method == "" {
			t.Errorf("UIRequest wrong: %+v", u)
		}
	}
}

func TestUIResponseMarshal(t *testing.T) {
	got, err := json.Marshal(CancelledUIResponse("uuid-1"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"type":"extension_ui_response","id":"uuid-1","cancelled":true}`
	if string(got) != want {
		t.Errorf("marshaled = %s, want %s", got, want)
	}
}
