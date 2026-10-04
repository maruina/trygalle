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

func TestCommandDataDecode(t *testing.T) {
	t.Run("all dispositions", func(t *testing.T) {
		for _, want := range []Disposition{DispositionStarted, DispositionQueued, DispositionHandled} {
			golden := `{"disposition":"` + string(want) + `"}`
			var d PromptData
			if err := json.Unmarshal([]byte(golden), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if d.Disposition != want {
				t.Errorf("disposition = %q, want %q", d.Disposition, want)
			}
		}
	})

	t.Run("clear_queue contents", func(t *testing.T) {
		golden := `{"steering":["Change direction"],"followUp":["Summarize when finished"]}`
		var d QueueContents
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(d.Steering) != 1 || d.Steering[0] != "Change direction" {
			t.Errorf("steering = %v", d.Steering)
		}
		if len(d.FollowUp) != 1 || d.FollowUp[0] != "Summarize when finished" {
			t.Errorf("followUp = %v", d.FollowUp)
		}
	})

	t.Run("new_session cancelled true and false", func(t *testing.T) {
		for _, want := range []bool{false, true} {
			golden := `{"cancelled":`
			if want {
				golden += `true`
			} else {
				golden += `false`
			}
			golden += `}`
			var d NewSessionData
			if err := json.Unmarshal([]byte(golden), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if d.Cancelled != want {
				t.Errorf("cancelled = %v, want %v", d.Cancelled, want)
			}
		}
	})

	t.Run("get_state full", func(t *testing.T) {
		golden := `{"model":{"id":"mock-model","name":"Mock","api":"openai-completions","provider":"mock","baseUrl":"http://127.0.0.1:1"},"thinkingLevel":"medium","isStreaming":true,"isCompacting":false,"sessionFile":"/tmp/sessions/s.jsonl","sessionId":"abc123","messageCount":5,"pendingMessageCount":2}`
		var d State
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.SessionID != "abc123" || d.SessionFile != "/tmp/sessions/s.jsonl" {
			t.Errorf("session fields wrong: %+v", d)
		}
		if !d.IsStreaming || d.IsCompacting {
			t.Errorf("streaming flags wrong: %+v", d)
		}
		if d.PendingMessageCount != 2 || d.MessageCount != 5 {
			t.Errorf("counts wrong: %+v", d)
		}
		if d.Model == nil || d.Model.ID != "mock-model" || d.Model.Provider != "mock" {
			t.Errorf("model wrong: %+v", d.Model)
		}
	})

	t.Run("get_state without model", func(t *testing.T) {
		golden := `{"isStreaming":false,"isCompacting":false,"sessionId":"abc123","messageCount":0,"pendingMessageCount":0}`
		var d State
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.Model != nil {
			t.Errorf("model = %+v, want nil", d.Model)
		}
	})

	t.Run("get_commands list", func(t *testing.T) {
		golden := `{"commands":[{"name":"fix-tests","description":"Fix failing tests","source":"prompt","sourceInfo":{"path":"/p/fix-tests.md","source":"local","scope":"project","origin":"top-level"}}]}`
		var d CommandsData
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(d.Commands) != 1 {
			t.Fatalf("commands = %d, want 1", len(d.Commands))
		}
		c := d.Commands[0]
		if c.Name != "fix-tests" || c.Description != "Fix failing tests" || c.Source != "prompt" {
			t.Errorf("command info wrong: %+v", c)
		}
		if len(c.SourceInfo) == 0 {
			t.Error("sourceInfo not preserved")
		}
	})

	t.Run("last assistant text null", func(t *testing.T) {
		golden := `{"text":null}`
		var d LastAssistantTextData
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.Text != nil {
			t.Errorf("text = %v, want nil", *d.Text)
		}
	})

	t.Run("last assistant text empty string", func(t *testing.T) {
		golden := `{"text":""}`
		var d LastAssistantTextData
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.Text == nil || *d.Text != "" {
			t.Errorf("text = %v, want empty string", d.Text)
		}
	})

	t.Run("last assistant text omitted", func(t *testing.T) {
		// Live Pi 1.0.1 returns data {} when no assistant text exists.
		golden := `{}`
		var d LastAssistantTextData
		if err := json.Unmarshal([]byte(golden), &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.Text != nil {
			t.Errorf("text = %v, want nil for omitted field", *d.Text)
		}
	})
}

func TestEventDecode(t *testing.T) {
	t.Run("agent_start", func(t *testing.T) {
		var e Event
		if err := json.Unmarshal([]byte(`{"type":"agent_start"}`), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventAgentStart {
			t.Errorf("type = %q", e.Type)
		}
	})

	t.Run("agent_end with messages and willRetry false", func(t *testing.T) {
		golden := `{"type":"agent_end","messages":[{"role":"assistant","content":[],"stopReason":"error","errorMessage":"boom","timestamp":1}],"willRetry":false}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventAgentEnd || e.WillRetry {
			t.Errorf("agent_end fields wrong: %+v", e)
		}
		if len(e.Messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(e.Messages))
		}
		m := e.Messages[0]
		if m.Role != "assistant" || m.StopReason != "error" || m.ErrorMessage != "boom" {
			t.Errorf("message wrong: %+v", m)
		}
	})

	t.Run("agent_settled", func(t *testing.T) {
		var e Event
		if err := json.Unmarshal([]byte(`{"type":"agent_settled"}`), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventAgentSettled {
			t.Errorf("type = %q", e.Type)
		}
	})

	t.Run("message_end with failed assistant message", func(t *testing.T) {
		golden := `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Provider error","timestamp":1}}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventMessageEnd || e.Message == nil {
			t.Fatalf("message_end wrong: %+v", e)
		}
		if e.Message.StopReason != "error" || e.Message.ErrorMessage != "Provider error" {
			t.Errorf("message wrong: %+v", e.Message)
		}
	})

	t.Run("compaction_start", func(t *testing.T) {
		var e Event
		if err := json.Unmarshal([]byte(`{"type":"compaction_start","reason":"threshold"}`), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventCompactionStart || e.Reason != "threshold" {
			t.Errorf("compaction_start wrong: %+v", e)
		}
	})

	t.Run("compaction_end", func(t *testing.T) {
		golden := `{"type":"compaction_end","reason":"threshold","result":{"summary":"..."},"aborted":false,"willRetry":false}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventCompactionEnd || e.Reason != "threshold" || len(e.Result) == 0 {
			t.Errorf("compaction_end wrong: %+v", e)
		}
	})

	t.Run("queue_update", func(t *testing.T) {
		golden := `{"type":"queue_update","steering":["one"],"followUp":["two","three"]}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(e.Steering) != 1 || e.Steering[0] != "one" {
			t.Errorf("steering wrong: %v", e.Steering)
		}
		if len(e.FollowUp) != 2 || e.FollowUp[1] != "three" {
			t.Errorf("followUp wrong: %v", e.FollowUp)
		}
	})

	t.Run("auto_retry_start", func(t *testing.T) {
		golden := `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"529 overloaded"}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventAutoRetryStart || e.ErrorMessage != "529 overloaded" {
			t.Errorf("auto_retry_start wrong: %+v", e)
		}
	})

	t.Run("unrelated fields are dropped", func(t *testing.T) {
		// message_update carries usage and assistantMessageEvent; the component
		// must not retain them.
		golden := `{"type":"message_update","usage":{"input":100,"output":1,"totalTokens":101},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello "}}`
		var e Event
		if err := json.Unmarshal([]byte(golden), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != "message_update" {
			t.Errorf("type = %q", e.Type)
		}
	})
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
