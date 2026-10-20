package runtime_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// fakeClient is a scripted PiClient: the test sets the canned results, and the
// fake records the commands the coordinator routes. It implements the record
// seams; it does not reimplement Pi behavior.
type fakeClient struct {
	promptDisposition rpc.Disposition
	promptErr         error
	// promptScript, when non-empty, takes precedence over the static fields
	// and is consumed in call order.
	promptScript []scriptedPrompt
	// onPrompt and onSteer run before the canned result returns, letting a
	// test play the events Pi emits while producing the response.
	onPrompt         func()
	onSteer          func()
	steerDisposition rpc.Disposition
	steerErr         error
	text             *string
	textErr          error

	// events carries the state events the test plays. An event pushed before
	// the canned prompt response returns matches the wire-order guarantee the
	// live tests pin for a synchronous extension run.
	events chan rpc.Event
	ui     chan rpc.UIRequest

	died    chan struct{}
	waitErr error

	activityMu sync.Mutex
	activity   rpc.Activity

	mu      sync.Mutex
	calls   []string
	answers []string // dialog ids answered cancelled
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		events: make(chan rpc.Event, 64),
		ui:     make(chan rpc.UIRequest, 16),
		died:   make(chan struct{}),
	}
}

func (f *fakeClient) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeClient) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// scriptedPrompt is one canned prompt outcome.
type scriptedPrompt struct {
	disposition rpc.Disposition
	err         error
}

func (f *fakeClient) Prompt(ctx context.Context, message string) (rpc.Disposition, error) {
	f.record("prompt")
	var d rpc.Disposition
	var err error
	if len(f.promptScript) == 0 {
		d, err = f.promptDisposition, f.promptErr
	} else {
		s := f.promptScript[0]
		f.promptScript = f.promptScript[1:]
		d, err = s.disposition, s.err
	}
	if f.onPrompt != nil {
		f.onPrompt()
	}
	return d, err
}

func (f *fakeClient) Steer(ctx context.Context, message string) (rpc.Disposition, error) {
	f.record("steer")
	if f.onSteer != nil {
		f.onSteer()
	}
	return f.steerDisposition, f.steerErr
}

func (f *fakeClient) GetState(ctx context.Context) (rpc.State, error) {
	f.record("get_state")
	return rpc.State{SessionID: "s-1"}, nil
}

func (f *fakeClient) GetCommands(ctx context.Context) ([]rpc.CommandInfo, error) {
	f.record("get_commands")
	return []rpc.CommandInfo{{Name: "echo", Source: "prompt"}}, nil
}

func (f *fakeClient) Abort(ctx context.Context) error {
	f.record("abort")
	return nil
}

func (f *fakeClient) ClearQueue(ctx context.Context) (rpc.QueueContents, error) {
	f.record("clear_queue")
	return rpc.QueueContents{}, nil
}

func (f *fakeClient) NewSession(ctx context.Context) (bool, error) {
	f.record("new_session")
	return false, nil
}

func (f *fakeClient) GetLastAssistantText(ctx context.Context) (*string, error) {
	f.record("get_last_assistant_text")
	if f.textErr != nil {
		return nil, f.textErr
	}
	return f.text, nil
}

func (f *fakeClient) Events() <-chan rpc.Event { return f.events }

func (f *fakeClient) LastActivity() rpc.Activity {
	f.activityMu.Lock()
	defer f.activityMu.Unlock()
	return f.activity
}

func (f *fakeClient) UIRequests() <-chan rpc.UIRequest { return f.ui }

func (f *fakeClient) AnswerUIDialog(ctx context.Context, id string) error {
	f.record("answer_ui_dialog:" + id)
	return nil
}

func (f *fakeClient) CloseStdin() error { f.record("close_stdin"); return nil }

func (f *fakeClient) Kill() error { f.record("kill"); return nil }

// Wait blocks until the test kills the fake pi, then returns the canned
// terminal condition, mirroring the caching contract of the real client.
func (f *fakeClient) Wait() error {
	<-f.died
	f.record("wait")
	return f.waitErr
}

func (f *fakeClient) die() { close(f.died) }

// recordingNotifier collects responses and notices on buffered channels so the
// coordinator event loop never blocks on delivery.
type recordingNotifier struct {
	responses chan string
	notices   chan runtime.Notice
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{
		responses: make(chan string, 16),
		notices:   make(chan runtime.Notice, 32),
	}
}

func (n *recordingNotifier) Response(ctx context.Context, text string) {
	n.responses <- text
}

func (n *recordingNotifier) Notice(ctx context.Context, notice runtime.Notice) {
	n.notices <- notice
}

func (n *recordingNotifier) waitForResponse(t *testing.T) string {
	t.Helper()
	select {
	case text := <-n.responses:
		return text
	case notice := <-n.notices:
		t.Fatalf("unexpected notice %q instead of a response", notice.Kind)
		return ""
	case <-time.After(2 * time.Second):
		t.Fatal("no response within 2s")
		return ""
	}
}

func (n *recordingNotifier) waitForNotice(t *testing.T, kind runtime.NoticeKind) runtime.Notice {
	t.Helper()
	for {
		select {
		case notice := <-n.notices:
			if notice.Kind == kind {
				return notice
			}
			t.Errorf("unexpected notice %q, want %q", notice.Kind, kind)
		case <-n.responses:
			t.Errorf("unexpected response before the %q notice", kind)
		case <-time.After(2 * time.Second):
			t.Fatalf("no %q notice within 2s", kind)
			return runtime.Notice{}
		}
	}
}

func newTestCoordinator(t *testing.T, f *fakeClient, n *recordingNotifier) *runtime.Coordinator {
	t.Helper()
	return runtime.NewCoordinator(f, n, runtime.DefaultConfig(), testLogger(t))
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startRunningOperation(t *testing.T, f *fakeClient, c *runtime.Coordinator) {
	t.Helper()
	f.promptDisposition = rpc.DispositionStarted
	if err := c.HandleUserInput(context.Background(), "start a run"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	f.events <- rpc.Event{Type: "agent_start"}
}

func TestSettledRunWithTextResponds(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("hello world")
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "hello world" {
		t.Errorf("response = %q, want %q", got, "hello world")
	}
	calls := f.getCalls()
	if !slices.Contains(calls, "get_last_assistant_text") {
		t.Errorf("settlement must query last assistant text; calls = %v", calls)
	}

	// The coordinator went back to idle: a second input routes fresh.
	f.text = new("again")
	startRunningOperation(t, f, c)
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "again" {
		t.Errorf("second response = %q, want %q", got, "again")
	}
}

func TestEmptyFinalTextEmitsEmptyResponse(t *testing.T) {
	for _, tt := range []struct {
		name string
		text *string
	}{
		{name: "null text", text: nil},
		{name: "empty string text", text: new("")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeClient()
			n := newRecordingNotifier()
			f.text = tt.text
			c := newTestCoordinator(t, f, n)
			defer c.Close()
			startRunningOperation(t, f, c)

			f.events <- rpc.Event{Type: "agent_settled"}
			if notice := n.waitForNotice(t, runtime.NoticeEmptyResponse); notice.Kind != runtime.NoticeEmptyResponse {
				t.Errorf("notice kind = %q", notice.Kind)
			}
			if !slices.Contains(f.getCalls(), "get_last_assistant_text") {
				t.Errorf("get_last_assistant_text not called; calls = %v", f.getCalls())
			}
		})
	}
}

func TestFailedRunClassifiedFromAssistantMessage(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.events <- rpc.Event{
		Type:    "message_end",
		Message: &rpc.Message{Role: "assistant", StopReason: "error", ErrorMessage: "provider down"},
	}
	f.events <- rpc.Event{Type: "agent_settled"}

	notice := n.waitForNotice(t, runtime.NoticeFailedRun)
	if notice.Text != "provider down" {
		t.Errorf("FailedRun text = %q, want Pi's error message", notice.Text)
	}
	if slices.Contains(f.getCalls(), "get_last_assistant_text") {
		t.Error("a failed run must not query last assistant text")
	}
}

func TestPromptErrorRetriesOnceWithSteerQueued(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.promptErr = errors.New("cannot prompt during compaction")
	f.steerDisposition = rpc.DispositionQueued
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "hi"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeSteerAcknowledged)
	if calls := f.getCalls(); !slices.Equal(calls, []string{"prompt", "steer"}) {
		t.Errorf("calls = %v, want [prompt, steer]", calls)
	}
}

func TestPromptAndSteerBothFailureRoutes(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.promptErr = errors.New("prompt rejected")
	f.steerErr = errors.New("steer rejected")
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "hi"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if notice.Text == "" {
		t.Error("RoutingFailure must carry the failure cause")
	}
	if calls := f.getCalls(); !slices.Equal(calls, []string{"prompt", "steer"}) {
		t.Errorf("calls = %v, want [prompt, steer]", calls)
	}
}

func TestPromptErrorSteerStartsOperation(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.promptErr = errors.New("cannot prompt during compaction")
	f.steerDisposition = rpc.DispositionStarted
	f.text = new("after compaction")
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "hi"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "after compaction" {
		t.Errorf("response = %q, want %q", got, "after compaction")
	}
}

func TestHandledWithoutRunCompletesImmediately(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("previous run text")
	f.promptDisposition = rpc.DispositionHandled
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/mock-notify"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	// The stale-text guard: the completion never queries Pi for text, so a
	// corrupt or stale last-assistant text cannot leak into the response.
	if notice := n.waitForNotice(t, runtime.NoticeEmptyResponse); notice.Kind != runtime.NoticeEmptyResponse {
		t.Errorf("notice kind = %q", notice.Kind)
	}
	if calls := f.getCalls(); slices.Contains(calls, "get_last_assistant_text") {
		t.Errorf("stale-text guard violated; calls = %v", calls)
	}
}

func TestHandledWithRunTracksToSettlement(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("extension run text")
	f.promptDisposition = rpc.DispositionHandled
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	// Pi emits agent_start before the handled response (pinned by the live
	// extension ordering test in the harness); the fake plays it while
	// producing the response, matching the client's
	// event-enqueued-before-response guarantee.
	f.onPrompt = func() {
		f.events <- rpc.Event{Type: "agent_start"}
	}
	if err := c.HandleUserInput(context.Background(), "/mock-run"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "extension run text" {
		t.Errorf("response = %q, want %q", got, "extension run text")
	}
}

func TestEventAfterHandledResponseIsUnassociated(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.promptDisposition = rpc.DispositionHandled
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/mock-notify"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeEmptyResponse)

	// Detached fire-and-forget work the extension scheduled after its handler
	// returned: observed, logged, and never attributed.
	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_settled"}

	// A later clean input produces exactly its own outcome; the detached
	// events added no settlement, response, or failure of their own.
	f.promptScript = []scriptedPrompt{
		{disposition: rpc.DispositionStarted},
	}
	f.text = new("second")
	if err := c.HandleUserInput(context.Background(), "proper input"); err != nil {
		t.Fatalf("second HandleUserInput: %v", err)
	}
	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "second" {
		t.Errorf("response = %q, want %q", got, "second")
	}
}

func TestFastCompletionBeforePromptResponse(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("quick")
	f.promptDisposition = rpc.DispositionStarted
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	// Enqueue the run's events before the caller can observe the prompt
	// response, matching the wire order Pi uses for a fast completion.
	if err := c.HandleUserInput(context.Background(), "hi"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_settled"}

	if got := n.waitForResponse(t); got != "quick" {
		t.Errorf("response = %q, want %q", got, "quick")
	}
}

func TestSecondPromptRoutesWhileFirstActive(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	// While the run is active, a second prompt steers and the reserved names
	// route, without spawning a second event consumer.
	f.promptScript = []scriptedPrompt{
		{disposition: rpc.DispositionQueued},
	}
	if err := c.HandleUserInput(context.Background(), "steer me"); err != nil {
		t.Fatalf("HandleUserInput while active: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeSteerAcknowledged)
	for _, cmd := range []string{"/status", "/abort", "/new"} {
		if err := c.HandleUserInput(context.Background(), cmd); err != nil {
			t.Errorf("%s while active: %v", cmd, err)
		}
	}

	// The first run still settles normally afterwards.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Errorf("response = %q, want %q", got, "final")
	}
}

// handleInputUntilFatal routes inputs until the coordinator turns fatal. The
// exit is observed through one bounded Wait goroutine, so an input racing the
// death can complete its routing before the loop ends.
func handleInputUntilFatal(t *testing.T, c *runtime.Coordinator, text string) error {
	t.Helper()
	var err error
	for range 100 {
		if err = c.HandleUserInput(context.Background(), text); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return err
}

func TestProcessExitWhileIdleIsFatal(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.waitErr = rpc.ErrProcessExited
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	f.die()
	if err := handleInputUntilFatal(t, c, "hi"); err == nil {
		t.Fatal("HandleUserInput after pi exit = nil error, want fatal")
	}
}

func TestProcessExitWhileRunningIsFatal(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.waitErr = rpc.ErrProcessExited
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.die()
	if err := handleInputUntilFatal(t, c, "second"); err == nil {
		t.Fatal("HandleUserInput after pi exit during run = nil error, want fatal")
	}
	// A dead process produces no settlement outcome.
	select {
	case text := <-n.responses:
		t.Errorf("unexpected response %q after process exit", text)
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q after process exit", notice.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReservedCommandsNeverReachPrompt(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	for _, cmd := range []string{"/status", "/abort", "/new"} {
		if err := c.HandleUserInput(context.Background(), cmd); err != nil {
			t.Fatalf("HandleUserInput(%q): %v", cmd, err)
		}
	}
	if calls := f.getCalls(); slices.Contains(calls, "prompt") {
		t.Errorf("reserved commands reached prompt; calls = %v", calls)
	}
}
