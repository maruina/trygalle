package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
)

const liveTimeout = 60 * time.Second

// sendPrompt routes one prompt and returns its disposition.
func sendPrompt(t *testing.T, client *rpc.Client, message string) rpc.Disposition {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.PromptCommand(message, "steer"))
	if err != nil {
		t.Fatalf("prompt %q: %v", message, err)
	}
	if !resp.Success {
		t.Fatalf("prompt %q failed: %s", message, resp.Error)
	}
	var data rpc.PromptData
	if err := resp.Decode(&data); err != nil {
		t.Fatalf("decode prompt data: %v", err)
	}
	return data.Disposition
}

// lastAssistantText waits for one command response and decodes its text; nil
// means no assistant text exists.
func lastAssistantText(t *testing.T, client *rpc.Client) *string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.GetLastAssistantTextCommand())
	if err != nil {
		t.Fatalf("get_last_assistant_text: %v", err)
	}
	if !resp.Success {
		t.Fatalf("get_last_assistant_text failed: %s", resp.Error)
	}
	var data rpc.LastAssistantTextData
	if err := resp.Decode(&data); err != nil {
		t.Fatalf("decode last assistant text: %v", err)
	}
	return data.Text
}

// runToSettled consumes state events until agent_settled and returns them in
// order. Completion detection keys only on agent_settled.
func runToSettled(t *testing.T, client *rpc.Client) []rpc.Event {
	t.Helper()
	var events []rpc.Event
	deadline := time.After(liveTimeout)
	for {
		select {
		case event := <-client.Events():
			events = append(events, event)
			if event.Type == rpc.EventAgentSettled {
				return events
			}
		case <-deadline:
			t.Fatalf("timeout waiting for agent_settled; saw %d events", len(events))
		}
	}
}

// lastAssistantOf inspects the last assistant message in an agent_end event.
// The plan pins failed-run classification on agent_end.messages.
func lastAssistantOf(end rpc.Event) (rpc.Message, bool) {
	for i := len(end.Messages) - 1; i >= 0; i-- {
		if end.Messages[i].Role == "assistant" {
			return end.Messages[i], true
		}
	}
	return rpc.Message{}, false
}

func TestLivePromptRoundTrip(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	const reply = "pinned scripted reply"
	model.RespondText(reply)
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := sendPrompt(t, client, "say hello"); d != rpc.DispositionStarted {
		t.Fatalf("idle prompt disposition = %q, want started", d)
	}
	runToSettled(t, client)

	// The round-trip completes only after settlement.
	if text := lastAssistantText(t, client); text == nil || *text != reply {
		t.Fatalf("last assistant text = %v, want %q", text, reply)
	}
}

func TestLiveRetryThenSettle(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.FailOnce() // first provider request 500s, Pi auto-retries
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := sendPrompt(t, client, "retry me"); d != rpc.DispositionStarted {
		t.Fatalf("prompt disposition = %q, want started", d)
	}
	events := runToSettled(t, client)

	firstEnd, ok := firstAgentEnd(events)
	if !ok || !firstEnd.WillRetry {
		t.Fatalf("first agent_end %+v must carry willRetry: true (retry then settle)", firstEnd)
	}
	// Completion detection keys only on agent_settled, which runToSettled
	// already required.
	if text := lastAssistantText(t, client); text == nil || *text != "hello from the mock model" {
		t.Fatalf("last assistant text = %v, want the retried reply", text)
	}
}

func firstAgentEnd(events []rpc.Event) (rpc.Event, bool) {
	for _, e := range events {
		if e.Type == rpc.EventAgentEnd {
			return e, true
		}
	}
	return rpc.Event{}, false
}

// TestLiveEmptyText pins the no-text representation. Observed against Pi 1.0.1:
// an assistant message with empty content yields get_last_assistant_text data
// `{}`, i.e. no text field (decodes to nil). The coordinator treats nil and ""
// identically; the EmptyResponse notice unit test lands with the runtime
// (Task 9).
func TestLiveEmptyText(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondEmptyText()
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := sendPrompt(t, client, "say nothing"); d != rpc.DispositionStarted {
		t.Fatalf("prompt disposition = %q, want started", d)
	}
	runToSettled(t, client)

	if text := lastAssistantText(t, client); text != nil {
		t.Fatalf("empty run last assistant text = %q, want null (no assistant text)", *text)
	}
}

func TestLiveFailedRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.FailAlways()
	client := harness.Start(t, bin, harness.WithModel(model))
	defer client.CloseStdin()

	if d := sendPrompt(t, client, "fail hard"); d != rpc.DispositionStarted {
		t.Fatalf("prompt disposition = %q, want started", d)
	}
	events := runToSettled(t, client)

	// The terminal agent_end after retries are exhausted carries the failed
	// assistant message with Pi's errorMessage.
	var lastEnd rpc.Event
	found := false
	for _, e := range events {
		if e.Type == rpc.EventAgentEnd {
			lastEnd, found = e, true
		}
	}
	if !found {
		t.Fatal("no agent_end observed")
	}
	message, ok := lastAssistantOf(lastEnd)
	if !ok {
		t.Fatalf("agent_end carries no assistant message")
	}
	if message.StopReason != "error" {
		t.Fatalf("terminal assistant stopReason = %q, want error", message.StopReason)
	}
	if message.ErrorMessage == "" {
		t.Fatal("failed run lacks Pi's errorMessage")
	}
}
