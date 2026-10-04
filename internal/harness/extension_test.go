package harness_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
)

var testExtension = filepath.Join("testdata", "agent", "extension.js")

// collectEvents consumes events until the predicate holds or the timeout
// elapses, returning what was seen.
func collectEvents(t *testing.T, client *rpc.Client, timeout time.Duration, until func([]rpc.Event) bool) []rpc.Event {
	t.Helper()
	var events []rpc.Event
	deadline := time.After(timeout)
	for {
		select {
		case e := <-client.Events():
			events = append(events, e)
			if until(events) {
				return events
			}
		case <-deadline:
			return events
		}
	}
}

// drainAll consumes every queued event so the sequence measured next starts
// cleanly.
func drainAll(client *rpc.Client) {
	for {
		select {
		case <-client.Events():
		default:
			return
		}
	}
}

func TestExtensionCommandsRegistered(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	defer client.CloseStdin()

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.GetCommandsCommand())
	if err != nil {
		t.Fatalf("get_commands: %v", err)
	}
	var commands rpc.CommandsData
	if err := resp.Decode(&commands); err != nil {
		t.Fatalf("decode get_commands: %v", err)
	}
	want := map[string]bool{"mock-dialog": false, "mock-notify": false, "mock-run": false}
	for _, c := range commands.Commands {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
			if c.Source != "extension" {
				t.Errorf("command %s source = %q, want extension", c.Name, c.Source)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("extension command %q not listed by get_commands", name)
		}
	}
}

// TestExtensionNotifyHandledWithoutRun pins the R8 negative case: a command
// that starts no run yields disposition "handled" with no preceding
// agent_start, and its notification arrives as a fire-and-forget UI request.
func TestExtensionNotifyHandledWithoutRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	defer client.CloseStdin()

	drainAll(client)
	if d := sendPrompt(t, client, "/mock-notify"); d != rpc.DispositionHandled {
		t.Fatalf("/mock-notify disposition = %q, want handled", d)
	}

	// No run follows this handled input.
	events := collectEvents(t, client, 2*time.Second, func([]rpc.Event) bool { return false })
	for _, e := range events {
		if e.Type == rpc.EventAgentStart || e.Type == rpc.EventAgentSettled {
			t.Fatalf("/mock-notify started a run (event %s observed)", e.Type)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case req := <-client.UIRequests():
		if req.Method != "notify" {
			t.Errorf("UI request method = %q, want notify", req.Method)
		}
	case <-ctx.Done():
		t.Fatal("no notify UI request observed")
	}
}

// TestExtensionRunStartsOwnRun pins R8: /mock-run calls pi.sendMessage with
// triggerTurn inside its handler, so Pi emits agent_start before the "handled"
// response and the run tracks to agent_settled.
func TestExtensionRunStartsOwnRun(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	defer client.CloseStdin()

	drainAll(client)
	if d := sendPrompt(t, client, "/mock-run"); d != rpc.DispositionHandled {
		t.Fatalf("/mock-run disposition = %q, want handled", d)
	}
	events := collectEvents(t, client, liveTimeout, func(events []rpc.Event) bool {
		return len(events) > 0 && events[len(events)-1].Type == rpc.EventAgentSettled
	})
	if len(events) == 0 || events[0].Type != rpc.EventAgentStart {
		t.Fatalf("first post-send event = %v, want agent_start before the handled run", events)
	}
	if text := lastAssistantText(t, client); text == nil || *text == "" {
		t.Fatalf("/mock-run produced no assistant text: %v", text)
	}
}
