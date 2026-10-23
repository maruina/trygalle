package runtime_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// errFakeGetState scripts a get_state failure for the status error path.
var errFakeGetState = errors.New("fake get_state failure")

// errFakeAbort scripts an abort-sequence failure.
var errFakeAbort = errors.New("fake abort failure")

// errFakeNewSession scripts a new-session-sequence failure.
var errFakeNewSession = errors.New("fake new_session failure")

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
	onPrompt            func()
	onSteer             func()
	steerDisposition    rpc.Disposition
	steerErr            error
	text                *string
	textErr             error
	state               rpc.State
	stateErr            error
	queue               rpc.QueueContents
	clearQueueErr       error
	abortErr            error
	newSessionCancelled bool
	newSessionErr       error

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
		state:  rpc.State{SessionID: "s-1"},
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
	return f.state, f.stateErr
}

func (f *fakeClient) GetCommands(ctx context.Context) ([]rpc.CommandInfo, error) {
	f.record("get_commands")
	return []rpc.CommandInfo{{Name: "echo", Source: "prompt"}}, nil
}

func (f *fakeClient) Abort(ctx context.Context) error {
	f.record("abort")
	return f.abortErr
}

func (f *fakeClient) ClearQueue(ctx context.Context) (rpc.QueueContents, error) {
	f.record("clear_queue")
	return f.queue, f.clearQueueErr
}

func (f *fakeClient) NewSession(ctx context.Context) (bool, error) {
	f.record("new_session")
	return f.newSessionCancelled, f.newSessionErr
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

func TestWait(t *testing.T) {
	c := newTestCoordinator(t, newFakeClient(), newRecordingNotifier())
	done := c.Done()
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop")
	}
	if err := c.Wait(); err == nil {
		t.Fatal("Wait = nil, want the stopped condition")
	}
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
	f.promptErr = &rpc.CommandError{Command: "prompt", Message: "cannot prompt during compaction"}
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
	f.promptErr = &rpc.CommandError{Command: "prompt", Message: "prompt rejected"}
	f.steerErr = &rpc.CommandError{Command: "steer", Message: "steer rejected"}
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
	f.promptErr = &rpc.CommandError{Command: "prompt", Message: "cannot prompt during compaction"}
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

func TestTransportFailureDoesNotRetryWithSteer(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	// A transport failure or a canceled caller may already have delivered
	// the prompt, so only an explicit Pi rejection is retried.
	f.promptErr = rpc.ErrProcessExited
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "hi"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if calls := f.getCalls(); !slices.Equal(calls, []string{"prompt"}) {
		t.Errorf("calls = %v, want [prompt] and no steer retry", calls)
	}
}

func TestCanceledCallerDoesNotStrandPending(t *testing.T) {
	// A caller that gives up must not leave the coordinator unable to accept
	// the next input (the begin/result handshake outlives the caller).
	for range 50 {
		f := newFakeClient()
		n := newRecordingNotifier()
		c := newTestCoordinator(t, f, n)

		ctx, cancel := context.WithCancel(context.Background())
		f.promptErr = context.Canceled
		f.steerErr = context.Canceled
		f.onPrompt = cancel
		_ = c.HandleUserInput(ctx, "hi")

		if err := c.HandleUserInput(context.Background(), "second"); err != nil {
			t.Fatalf("input after a canceled caller = %v, want the input to route", err)
		}
		if calls := f.getCalls(); !slices.Equal(calls, []string{"prompt", "prompt"}) {
			t.Fatalf("calls = %v, want [prompt prompt] with no steer retry", calls)
		}
		c.Close()
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

func TestHandledMidRunEmitsExtensionHandled(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("active run text")
	f.promptScript = []scriptedPrompt{
		{disposition: rpc.DispositionStarted},
		{disposition: rpc.DispositionHandled},
	}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	// An extension command consumed the input while the run was active: it
	// executed, started no run of its own, and the run continues.
	if err := c.HandleUserInput(context.Background(), "/mock-notify"); err != nil {
		t.Fatalf("HandleUserInput: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeExtensionHandled)

	// The active run keeps its attribution: its settlement still responds.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "active run text" {
		t.Errorf("response = %q, want %q", got, "active run text")
	}
}

func TestUnexpectedDispositionEmitsRoutingFailure(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.promptDisposition = rpc.Disposition("teleported")
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "hi"); err == nil {
		t.Error("HandleUserInput with an unknown disposition = nil error, want an error")
	}
	notice := n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if !strings.Contains(notice.Text, "teleported") {
		t.Errorf("RoutingFailure text = %q, want the unexpected disposition", notice.Text)
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

	// While the run is active, a second prompt steers and /status routes,
	// without spawning a second event consumer.
	f.promptScript = []scriptedPrompt{
		{disposition: rpc.DispositionQueued},
	}
	if err := c.HandleUserInput(context.Background(), "steer me"); err != nil {
		t.Fatalf("HandleUserInput while active: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeSteerAcknowledged)
	// /status reports without disturbing the run; /new still stubs out to
	// Task 15. /abort now terminates the run (covered by the abort tests),
	// so it no longer belongs in this settle-normally flow.
	if err := c.HandleUserInput(context.Background(), "/status"); err != nil {
		t.Fatalf("/status while active: %v", err)
	}
	n.waitForNotice(t, runtime.NoticeStatus)
	// /abort now terminates the run and /new supersedes it (both covered by
	// their own tests), so neither belongs in this settle-normally flow.

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

func TestStatusIdleReportsScriptedState(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.state = rpc.State{
		SessionID:           "sess-42",
		IsStreaming:         false,
		MessageCount:        3,
		PendingMessageCount: 0,
		Model:               &rpc.Model{ID: "mock-model"},
	}
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/status"); err != nil {
		t.Fatalf("HandleUserInput(/status): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeStatus)
	if notice.State == nil {
		t.Fatal("Status notice carries no state")
	}
	if notice.State.SessionID != "sess-42" || notice.State.IsStreaming {
		t.Errorf("Status state = %+v, want sess-42 idle", notice.State)
	}
	if notice.State.Model == nil || notice.State.Model.ID != "mock-model" {
		t.Errorf("Status model = %+v, want mock-model", notice.State.Model)
	}
	if calls := f.getCalls(); !slices.Equal(calls, []string{"get_state"}) {
		t.Errorf("calls = %v, want [get_state] and no prompt/steer", calls)
	}
}

func TestStatusMidRunLeavesOperationUndisturbed(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	f.state = rpc.State{SessionID: "sess-1", IsStreaming: true}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)
	before := len(f.getCalls())

	if err := c.HandleUserInput(context.Background(), "/status"); err != nil {
		t.Fatalf("/status while active: %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeStatus)
	if notice.State == nil || !notice.State.IsStreaming {
		t.Errorf("Status state = %+v, want streaming sess-1", notice.State)
	}
	for _, call := range f.getCalls()[before:] {
		if call == "prompt" || call == "steer" {
			t.Errorf("status query submitted %q; calls = %v", call, f.getCalls())
		}
	}

	// The active run still settles into its own response afterwards.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Errorf("response = %q, want %q", got, "final")
	}
}

func TestStatusErrorEmitsRoutingFailure(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.stateErr = errFakeGetState
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/status"); err != nil {
		t.Fatalf("HandleUserInput(/status): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if notice.Text == "" {
		t.Error("RoutingFailure must carry the get_state failure cause")
	}
	select {
	case extra := <-n.notices:
		t.Errorf("unexpected extra notice %q after RoutingFailure", extra.Kind)
	case extra := <-n.responses:
		t.Errorf("unexpected response %q after failed status", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAbortIdleRunsSequenceAndReports(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.queue = rpc.QueueContents{Steering: []string{"steered"}, FollowUp: []string{"follow"}}
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	// Esc on an idle session still runs the sequence and reports.
	if err := c.HandleUserInput(context.Background(), "/abort"); err != nil {
		t.Fatalf("HandleUserInput(/abort): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeAbortReport)
	if notice.Text != "no operation was active" {
		t.Errorf("idle AbortReport text = %q, want the no-operation marker", notice.Text)
	}
	if !slices.Equal(notice.Dropped.Steering, []string{"steered"}) ||
		!slices.Equal(notice.Dropped.FollowUp, []string{"follow"}) {
		t.Errorf("AbortReport dropped = %+v, want the cleared queue", notice.Dropped)
	}
	assertClearQueueBeforeAbort(t, f)
}

func assertClearQueueBeforeAbort(t *testing.T, f *fakeClient) {
	t.Helper()
	calls := f.getCalls()
	clearIdx := slices.Index(calls, "clear_queue")
	abortIdx := slices.Index(calls, "abort")
	if clearIdx < 0 || abortIdx < 0 {
		t.Fatalf("abort sequence calls = %v, want clear_queue then abort", calls)
	}
	if clearIdx > abortIdx {
		t.Errorf("abort sequence order = %v, want clear_queue before abort", calls)
	}
}

func TestAbortMidRunTerminatesOperation(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.queue = rpc.QueueContents{Steering: []string{"steered"}}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	// The abort outcome arrives before the settle: it is the operation's
	// single terminal outcome.
	if err := c.HandleUserInput(context.Background(), "/abort"); err != nil {
		t.Fatalf("/abort while active: %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeAbortReport)
	if notice.Text != "" {
		t.Errorf("mid-run AbortReport text = %q, want empty (an operation was aborted)", notice.Text)
	}
	if !slices.Equal(notice.Dropped.Steering, []string{"steered"}) {
		t.Errorf("AbortReport dropped = %+v, want the cleared steering", notice.Dropped)
	}
	assertClearQueueBeforeAbort(t, f)

	// A late settle from the aborted run is logged, never attributed: no
	// second outcome follows the abort report.
	f.events <- rpc.Event{Type: "agent_settled"}
	select {
	case text := <-n.responses:
		t.Errorf("unexpected response %q after the abort report", text)
	case extra := <-n.notices:
		t.Errorf("unexpected notice %q after the abort report", extra.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAbortAfterSettleReportsIdle(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	f.queue = rpc.QueueContents{FollowUp: []string{"leftover"}}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	// The settle arrives before the abort outcome: the operation reports its
	// normal terminal outcome first.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Fatalf("response = %q, want %q", got, "final")
	}

	if err := c.HandleUserInput(context.Background(), "/abort"); err != nil {
		t.Fatalf("HandleUserInput(/abort): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeAbortReport)
	if notice.Text != "no operation was active" {
		t.Errorf("post-settle AbortReport text = %q, want the no-operation marker", notice.Text)
	}
	if !slices.Equal(notice.Dropped.FollowUp, []string{"leftover"}) {
		t.Errorf("AbortReport dropped = %+v, want the cleared follow-up", notice.Dropped)
	}
}

func TestAbortSequenceErrorEmitsRoutingFailure(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	f.abortErr = errFakeAbort
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	if err := c.HandleUserInput(context.Background(), "/abort"); err != nil {
		t.Fatalf("HandleUserInput(/abort): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if !strings.Contains(notice.Text, "fake abort failure") {
		t.Errorf("RoutingFailure text = %q, want the abort failure cause", notice.Text)
	}

	// The failed sequence leaves the operation running: its settle is still
	// attributed normally.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Errorf("response = %q, want %q", got, "final")
	}
}

func TestNewSessionMidRunSupersedes(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.queue = rpc.QueueContents{Steering: []string{"steered"}, FollowUp: []string{"follow"}}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeNewSessionReport)
	if notice.Text != "" {
		t.Errorf("switched NewSessionReport text = %q, want empty (no cancellation)", notice.Text)
	}
	if !slices.Equal(notice.Dropped.Steering, []string{"steered"}) ||
		!slices.Equal(notice.Dropped.FollowUp, []string{"follow"}) {
		t.Errorf("NewSessionReport dropped = %+v, want the cleared queue", notice.Dropped)
	}
	calls := f.getCalls()
	clearIdx, newIdx := slices.Index(calls, "clear_queue"), slices.Index(calls, "new_session")
	if clearIdx < 0 || newIdx < 0 || clearIdx > newIdx {
		t.Errorf("new-session sequence calls = %v, want clear_queue before new_session", calls)
	}

	// Trailing events from the superseded run are logged, never attributed:
	// a failed assistant message in a trailing agent_end produces no
	// FailedRun, and the trailing settle produces no outcome.
	f.events <- rpc.Event{
		Type: "agent_end",
		Messages: []rpc.Message{
			{Role: "assistant", StopReason: "error", ErrorMessage: "old run died"},
		},
	}
	f.events <- rpc.Event{Type: "agent_settled"}
	select {
	case text := <-n.responses:
		t.Errorf("unexpected response %q from trailing superseded events", text)
	case extra := <-n.notices:
		t.Errorf("unexpected notice %q from trailing superseded events", extra.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNewSessionCancelledKeepsSession(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	f.newSessionCancelled = true
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeNewSessionReport)
	if notice.Text != "session switch cancelled" {
		t.Errorf("cancelled NewSessionReport text = %q, want the cancellation marker", notice.Text)
	}

	// The current session (and its active run) continues: the run still
	// settles into its own response afterwards.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Errorf("response = %q, want %q", got, "final")
	}
}

func TestNewSessionEmptyQueue(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeNewSessionReport)
	if len(notice.Dropped.Steering) != 0 || len(notice.Dropped.FollowUp) != 0 {
		t.Errorf("NewSessionReport dropped = %+v, want empty", notice.Dropped)
	}
}

func TestPromptAfterNewIgnoresLateSettle(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("new run text")
	f.promptDisposition = rpc.DispositionStarted
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	n.waitForNotice(t, runtime.NoticeNewSessionReport)

	// A prompt issued right after /new starts a new operation while the
	// supersede suppression is still armed (no agent_start yet).
	if err := c.HandleUserInput(context.Background(), "new input"); err != nil {
		t.Fatalf("HandleUserInput after /new: %v", err)
	}

	// A late settle from the superseded run arrives before the new run
	// starts: it is dropped, so the new run keeps its attribution.
	f.events <- rpc.Event{Type: "agent_settled"}
	select {
	case text := <-n.responses:
		t.Fatalf("late superseded settle produced response %q", text)
	case extra := <-n.notices:
		t.Fatalf("late superseded settle produced notice %q", extra.Kind)
	case <-time.After(100 * time.Millisecond):
	}

	f.events <- rpc.Event{Type: "agent_start"}
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "new run text" {
		t.Errorf("response = %q, want %q", got, "new run text")
	}
}

func TestNewSessionSequenceErrorEmitsRoutingFailure(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.text = new("final")
	f.newSessionErr = errFakeNewSession
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	if err := c.HandleUserInput(context.Background(), "/new"); err != nil {
		t.Fatalf("HandleUserInput(/new): %v", err)
	}
	notice := n.waitForNotice(t, runtime.NoticeRoutingFailure)
	if !strings.Contains(notice.Text, "fake new_session failure") {
		t.Errorf("RoutingFailure text = %q, want the new_session failure cause", notice.Text)
	}

	// The failed sequence leaves the operation running: its settle is still
	// attributed normally.
	f.events <- rpc.Event{Type: "agent_settled"}
	if got := n.waitForResponse(t); got != "final" {
		t.Errorf("response = %q, want %q", got, "final")
	}
}
