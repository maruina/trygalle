package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// compactionLiveTimeout bounds every live compaction wait. Each compaction
// window is one delayed mock response, so this leaves room for process startup.
const compactionLiveTimeout = 90 * time.Second

// compactionModelDelay holds the model response open long enough to reach Pi
// with a prompt and a steer inside the window it creates.
const compactionModelDelay = 3 * time.Second

// compactionHistoryText is large enough that Pi 1.3.0 has a range to
// summarize: compaction keeps the most recent ~20k tokens, so a smaller
// history reports "Nothing to compact (session too small)".
func compactionHistoryText() string {
	return strings.Repeat("compaction history filler. ", 4000)
}

// TestLiveCompactionPromptRejectedSteerQueued pins R6 against the real Pi: in
// a manual compaction window `prompt` is rejected while bare `steer` queues,
// so the coordinator's single retry keeps the message with Pi instead of
// dropping it, and the queued message is delivered by the next run.
//
// Trigger note (probed on Pi 1.3.0): a mid-run `compact` command is accepted
// but aborts the run before it compacts, and an aborted run delivers no queued
// steering. Manual compaction on an idle session is therefore the only live
// window where `prompt` reports the documented compaction error; the run that
// delivers the queued message follows the window.
func TestLiveCompactionPromptRejectedSteerQueued(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText(compactionHistoryText())
	client := harness.Start(t, bin, harness.WithModel(model))
	capture := &logCapture{}
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), capture.logger())
	defer c.Close()

	// History the manual compaction can summarize.
	if err := c.HandleUserInput(context.Background(), "build compaction history"); err != nil {
		t.Fatalf("HandleUserInput(history): %v", err)
	}
	if text := awaitLiveResponse(t, n); text == "" {
		t.Fatal("history run produced no response")
	}

	model.RespondText("answer after the compaction")
	model.Delay(compactionModelDelay)

	// Manual compaction opens the prompt-rejection window.
	compactErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), compactionLiveTimeout)
		defer cancel()
		resp, err := client.Send(ctx, rpc.Command{Type: "compact"})
		if err == nil && !resp.Success {
			err = fmt.Errorf("compact failed: %s", resp.Error)
		}
		compactErr <- err
	}()
	awaitPiState(t, client, "compaction to start", func(s rpc.State) bool { return s.IsCompacting })

	// The coordinator's compaction fallback keeps the message with Pi: prompt
	// is rejected, the bare-steer retry is accepted as queued.
	const steered = "steer queued in the compaction window"
	if err := c.HandleUserInput(context.Background(), steered); err != nil {
		t.Fatalf("HandleUserInput during compaction = %v, want the fallback to succeed", err)
	}
	rec := capture.waitFor(t, "prompt rejected; retrying once with bare steer")
	if rec.attrs["error"] == "" {
		t.Errorf("rejection log carries no error field: %v", rec.attrs)
	}
	awaitLiveNotice(t, n, runtime.NoticeSteerAcknowledged)

	if err := <-compactErr; err != nil {
		t.Fatalf("manual compact: %v", err)
	}
	awaitPiState(t, client, "compaction to end", func(s rpc.State) bool { return !s.IsCompacting })
	if state := liveGetState(t, client); state.PendingMessageCount != 1 {
		t.Fatalf("pending after compaction = %d, want the queued steering to survive the window", state.PendingMessageCount)
	}

	// The next run delivers the queued message and still settles once.
	if err := c.HandleUserInput(context.Background(), "run that delivers the queued steer"); err != nil {
		t.Fatalf("HandleUserInput(delivering run): %v", err)
	}
	if text := awaitLiveResponse(t, n); text != "answer after the compaction" {
		t.Errorf("delivering run response = %q, want the scripted answer", text)
	}
	if texts := liveUserMessages(t, client); !slices.Contains(texts, steered) {
		t.Errorf("session user messages lack the steered text: %v", texts)
	}
	if state := liveGetState(t, client); state.PendingMessageCount != 0 {
		t.Errorf("pending after delivery = %d, want an empty queue", state.PendingMessageCount)
	}
}

// TestLiveCompactionOverflowMidRunDelivery pins design contract test 2 against
// the real Pi: a compaction window inside a live run keeps the operation
// running, a steered message joins the run, and the run settles once after the
// steered message is delivered.
//
// Trigger: the mock model answers one request with a context-overflow error,
// so Pi compacts the run and retries the turn once. The delayed summarization
// call is the window. In this window `prompt{steer}` queues rather than
// failing: Pi 1.3.0 rejects prompts only for manual compaction.
func TestLiveCompactionOverflowMidRunDelivery(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText(compactionHistoryText())
	client := harness.Start(t, bin, harness.WithModel(model))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "build compaction history"); err != nil {
		t.Fatalf("HandleUserInput(history): %v", err)
	}
	if text := awaitLiveResponse(t, n); text == "" {
		t.Fatal("history run produced no response")
	}

	model.RespondText("answer after the compaction")
	model.OverflowOnce()
	model.Delay(compactionModelDelay)

	if err := c.HandleUserInput(context.Background(), "run held open by overflow recovery"); err != nil {
		t.Fatalf("HandleUserInput(run): %v", err)
	}
	awaitPiState(t, client, "the overflow compaction to start", func(s rpc.State) bool {
		return s.IsCompacting && s.IsStreaming
	})

	// The operation is still running, so the input joins it as steering.
	const steered = "steer sent inside the overflow compaction window"
	if err := c.HandleUserInput(context.Background(), steered); err != nil {
		t.Fatalf("HandleUserInput(steer): %v", err)
	}
	awaitLiveNotice(t, n, runtime.NoticeSteerAcknowledged)
	if state := liveGetState(t, client); !state.IsCompacting || state.PendingMessageCount != 1 {
		t.Fatalf("mid-window state = %+v, want compacting with the steered message queued", state)
	}

	if text := awaitLiveResponse(t, n); text != "answer after the compaction" {
		t.Errorf("run response = %q, want the scripted answer", text)
	}
	if texts := liveUserMessages(t, client); !slices.Contains(texts, steered) {
		t.Errorf("session user messages lack the steered text: %v", texts)
	}
	if state := liveGetState(t, client); state.PendingMessageCount != 0 {
		t.Errorf("pending after settle = %d, want an empty queue", state.PendingMessageCount)
	}
}

// TestLiveMidRunExtensionCommandHandled pins the third mid-run disposition
// (design contract test 1): an extension command sent during a run reports
// "handled" and executes immediately, well before the held-open run settles.
func TestLiveMidRunExtensionCommandHandled(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.Delay(compactionModelDelay) // hold the run open across the command
	client := harness.Start(t, bin, harness.WithModel(model), harness.WithExtension(testExtension))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "slow run for the extension command"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}

	start := time.Now()
	if err := c.HandleUserInput(context.Background(), "/mock-run"); err != nil {
		t.Fatalf("HandleUserInput(/mock-run): %v", err)
	}
	if elapsed := time.Since(start); elapsed >= compactionModelDelay {
		t.Errorf("/mock-run took %v mid-run, want an immediate handled response", elapsed)
	}
	awaitLiveNotice(t, n, runtime.NoticeExtensionHandled)

	if text := awaitLiveResponse(t, n); text == "" {
		t.Error("run response is empty after the handled extension command")
	}
}

// awaitPiState polls the Pi-reported state until pred holds. Polling keeps the
// event stream with the coordinator, which is its only consumer.
func awaitPiState(t *testing.T, client *rpc.Client, what string, pred func(rpc.State) bool) rpc.State {
	t.Helper()
	deadline := time.Now().Add(compactionLiveTimeout)
	for {
		state := liveGetState(t, client)
		if pred(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// liveUserMessages returns the text of every user message in Pi's session.
func liveUserMessages(t *testing.T, client *rpc.Client) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), compactionLiveTimeout)
	defer cancel()
	resp, err := client.Send(ctx, rpc.Command{Type: "get_messages"})
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	if !resp.Success {
		t.Fatalf("get_messages failed: %s", resp.Error)
	}
	var data struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := resp.Decode(&data); err != nil {
		t.Fatalf("decode get_messages data: %v", err)
	}
	var texts []string
	for _, m := range data.Messages {
		if m.Role != "user" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "text" {
				texts = append(texts, block.Text)
			}
		}
	}
	return texts
}

// awaitLiveResponse consumes notices until a run response arrives.
func awaitLiveResponse(t *testing.T, n *recordingNotifier) string {
	t.Helper()
	select {
	case text := <-n.responses:
		return text
	case notice := <-n.notices:
		t.Fatalf("unexpected notice %q instead of a run response", notice.Kind)
		return ""
	case <-time.After(compactionLiveTimeout):
		t.Fatal("no run response within the live bound")
		return ""
	}
}

// awaitLiveNotice consumes notifications until the wanted notice arrives.
func awaitLiveNotice(t *testing.T, n *recordingNotifier, kind runtime.NoticeKind) runtime.Notice {
	t.Helper()
	deadline := time.After(compactionLiveTimeout)
	for {
		select {
		case notice := <-n.notices:
			if notice.Kind == kind {
				return notice
			}
			t.Fatalf("unexpected notice %q, want %q", notice.Kind, kind)
		case text := <-n.responses:
			t.Fatalf("unexpected response %q before the %q notice", text, kind)
		case <-deadline:
			t.Fatalf("no %q notice within the live bound", kind)
		}
	}
}
