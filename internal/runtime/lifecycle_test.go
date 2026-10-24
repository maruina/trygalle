package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// errValidation scripts a get_state failure strike.
var errValidation = errors.New("scripted validation failure")

// startClient scripts one Pi child for the startup ladder tests. It records
// the lifecycle calls the ladder makes so the tests can assert the stop
// ordering between attempts; the coordinator-facing methods panic, because
// the ladder only validates and stops a child.
type startClient struct {
	state     rpc.State
	stateErr  error
	stateHang bool // GetState blocks until its ctx is done, then returns ctx.Err()
	commands  []rpc.CommandInfo
	cmdErr    error

	waitErr    error
	waitGate   chan struct{} // when non-nil, Wait blocks until it closes
	waitKilled chan struct{} // when non-nil, Kill closes it, modeling a killed child

	mu          sync.Mutex
	stdinClosed bool
	killed      bool

	waitOnce     sync.Once
	waitReturned chan struct{} // closed when Wait returns once
}

func newStartClient() *startClient {
	return &startClient{
		state:        rpc.State{SessionID: "s-scripted"},
		commands:     []rpc.CommandInfo{{Name: "echo", Source: "prompt"}},
		waitReturned: make(chan struct{}),
	}
}

func (c *startClient) exited() <-chan struct{} { return c.waitReturned }

func (c *startClient) wasStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdinClosed
}

func (c *startClient) wasKilled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killed
}

// The coordinator-facing methods below are never called by the startup
// ladder; a call would mean the ladder started routing inputs, which it must
// not do before validation succeeds.
func (c *startClient) Prompt(ctx context.Context, message string) (rpc.Disposition, error) {
	panic("startClient: unexpected prompt during startup")
}

func (c *startClient) Steer(ctx context.Context, message string) (rpc.Disposition, error) {
	panic("startClient: unexpected steer during startup")
}

func (c *startClient) Abort(ctx context.Context) error {
	panic("startClient: unexpected abort during startup")
}

func (c *startClient) ClearQueue(ctx context.Context) (rpc.QueueContents, error) {
	panic("startClient: unexpected clear_queue during startup")
}

func (c *startClient) NewSession(ctx context.Context) (bool, error) {
	panic("startClient: unexpected new_session during startup")
}

func (c *startClient) GetLastAssistantText(ctx context.Context) (*string, error) {
	panic("startClient: unexpected get_last_assistant_text during startup")
}

func (c *startClient) Events() <-chan rpc.Event { panic("startClient: unexpected event subscription") }

func (c *startClient) LastActivity() rpc.Activity { panic("startClient: unexpected activity query") }

func (c *startClient) UIRequests() <-chan rpc.UIRequest {
	panic("startClient: unexpected ui subscription")
}

func (c *startClient) AnswerUIDialog(ctx context.Context, id string) error {
	panic("startClient: unexpected dialog answer during startup")
}

func (c *startClient) GetState(ctx context.Context) (rpc.State, error) {
	if c.stateHang {
		<-ctx.Done()
		return rpc.State{}, ctx.Err()
	}
	return c.state, c.stateErr
}

func (c *startClient) GetCommands(ctx context.Context) ([]rpc.CommandInfo, error) {
	return c.commands, c.cmdErr
}

func (c *startClient) CloseStdin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdinClosed = true
	return nil
}

func (c *startClient) Kill() error {
	c.mu.Lock()
	c.killed = true
	c.mu.Unlock()
	if c.waitKilled != nil {
		close(c.waitKilled)
	}
	return nil
}

func (c *startClient) Wait() error {
	if c.waitGate != nil {
		<-c.waitGate
	}
	c.waitOnce.Do(func() { close(c.waitReturned) })
	return c.waitErr
}

// startScript feeds the ladder a scripted child per start call and asserts
// inside the start function that the previous child has exited: at most one
// Pi child may own the session directory at a time.
type startScript struct {
	children []*startClient
	failAt   map[int]error // start call index (0-based) -> start error

	starts   int
	argsSeen [][]string
}

// start returns the StartFunc under test.
func (s *startScript) start(t *testing.T) runtime.StartFunc {
	t.Helper()
	return func(args []string) (runtime.PiClient, error) {
		i := s.starts
		s.starts++
		s.argsSeen = append(s.argsSeen, args)
		if err, ok := s.failAt[i]; ok {
			return nil, err
		}
		if i > 0 {
			prev := s.children[i-1]
			select {
			case <-prev.exited():
			default:
				t.Errorf("start %d began before child %d exited", i+1, i)
			}
		}
		return s.children[i], nil
	}
}

// lifecycleConfig returns a config with a short validation timeout so deadline
// paths stay fast; it is still long enough for the immediate fake calls.
func lifecycleConfig(args ...string) runtime.Config {
	cfg := runtime.DefaultConfig()
	cfg.Args = args
	cfg.StartupValidationTimeout = 200 * time.Millisecond
	return cfg
}

func TestStartupOneStrikeRecovers(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient()},
	}
	script.children[0].stateErr = errValidation

	cfg := lifecycleConfig("--mode", "rpc", "--continue", "--session-dir", "dir")
	n := newRecordingNotifier()
	client, err := runtime.Start(context.Background(), script.start(t), n, cfg, testLogger(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if script.starts != 2 {
		t.Fatalf("start calls = %d, want one strike then recovery", script.starts)
	}
	if client != runtime.PiClient(script.children[1]) {
		t.Fatal("Start returned the failed child's client, want the retry's")
	}
	for i, args := range script.argsSeen {
		if strings.Join(args, " ") != strings.Join(cfg.Args, " ") {
			t.Errorf("start %d argv = %v, want the full args %v", i+1, args, cfg.Args)
		}
	}
	if child := script.children[0]; !child.wasStopped() {
		t.Error("the failed child was not stopped before the retry")
	}
	select {
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q on the one-strike recovery", notice.Kind)
	case <-n.responses:
		t.Error("unexpected response on the one-strike recovery")
	default:
	}
}

func TestStartupExitsDuringValidationStrike(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient()},
	}
	script.children[0].stateErr = rpc.ErrProcessExited
	script.children[0].waitErr = rpc.ErrProcessExited

	cfg := lifecycleConfig("--mode", "rpc", "--continue")
	if _, err := runtime.Start(context.Background(), script.start(t), newRecordingNotifier(), cfg, testLogger(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if script.starts != 2 {
		t.Fatalf("start calls = %d, want the retry after the mid-validation exit", script.starts)
	}
	if child := script.children[0]; !child.wasStopped() {
		t.Error("the exited child was not stopped before the retry")
	}
}

func TestStartupDeadlineExpiryIsAStrike(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient()},
	}
	script.children[0].stateHang = true

	cfg := lifecycleConfig("--mode", "rpc")
	if _, err := runtime.Start(context.Background(), script.start(t), newRecordingNotifier(), cfg, testLogger(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if script.starts != 2 {
		t.Fatalf("start calls = %d, want the deadline expiry to strike once and the retry to recover", script.starts)
	}
}

func TestStartupTwoStrikesStartDegraded(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient(), newStartClient()},
	}
	script.children[0].stateErr = errValidation
	script.children[1].stateErr = errValidation

	cfg := lifecycleConfig("--mode", "rpc", "--continue", "--session-dir", "dir")
	n := newRecordingNotifier()
	if _, err := runtime.Start(context.Background(), script.start(t), n, cfg, testLogger(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if script.starts != 3 {
		t.Fatalf("start calls = %d, want two strikes then the degraded start", script.starts)
	}
	degraded := strings.Join(cfg.DegradedArgs(), " ")
	for i, args := range script.argsSeen {
		want := strings.Join(cfg.Args, " ")
		if i == 2 {
			want = degraded
		}
		if strings.Join(args, " ") != want {
			t.Errorf("start %d argv = %v, want %v", i+1, args, want)
		}
	}
	if degraded == strings.Join(cfg.Args, " ") {
		t.Fatal("the degraded argv equals the full argv; --continue was not dropped")
	}
	notice := n.waitForNotice(t, runtime.NoticeSessionResumeFailed)
	if notice.Text == "" || !strings.Contains(notice.Text, errValidation.Error()) {
		t.Errorf("SessionResumeFailed text = %q, want the second strike's cause", notice.Text)
	}
}

func TestStartupDegradedFailureIsFatal(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient(), newStartClient()},
	}
	for _, child := range script.children {
		child.stateErr = errValidation
	}

	cfg := lifecycleConfig("--mode", "rpc", "--continue")
	n := newRecordingNotifier()
	if _, err := runtime.Start(context.Background(), script.start(t), n, cfg, testLogger(t)); err == nil {
		t.Fatal("Start = nil error, want the fatal degraded failure")
	}
	if script.starts != 3 {
		t.Fatalf("start calls = %d, want the degraded start to be the last attempt", script.starts)
	}
	for i, child := range script.children {
		if !child.wasStopped() {
			t.Errorf("child %d was not stopped before Start returned", i+1)
		}
	}
	select {
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q on the fatal degraded failure", notice.Kind)
	default:
	}
}

func TestStartupHungChildKilledBeforeRetry(t *testing.T) {
	script := &startScript{
		children: []*startClient{newStartClient(), newStartClient()},
	}
	hung := script.children[0]
	hung.stateHang = true
	hung.waitGate = make(chan struct{})
	hung.waitKilled = hung.waitGate // only the kill releases the hung child

	cfg := lifecycleConfig("--mode", "rpc")
	if _, err := runtime.Start(context.Background(), script.start(t), newRecordingNotifier(), cfg, testLogger(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if script.starts != 2 {
		t.Fatalf("start calls = %d, want the hung child stopped before the retry", script.starts)
	}
	if !hung.wasKilled() {
		t.Error("the hung child was not killed before the retry")
	}
	select {
	case <-hung.exited():
	default:
		t.Error("the hung child had not exited when the retry started")
	}
}

// TestProtocolErrorWhileIdleIsFatal pins the coordinator fatal wiring for a
// protocol integrity failure (R16): the client's cached terminal condition is
// the runtime's fatal error, distinct from a pi parse response, which the rpc
// client logs without dying (TestParseResponseLoggedNotFatal).
func TestProtocolErrorWhileIdleIsFatal(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.waitErr = &rpc.ProtocolError{Err: errors.New("garbage record")}
	c := newTestCoordinator(t, f, n)
	defer c.Close()

	f.die()
	if err := handleInputUntilFatal(t, c, "hi"); err == nil {
		t.Fatal("HandleUserInput after a protocol integrity failure = nil error, want fatal")
	}
}

func TestProtocolErrorWhileRunningIsFatal(t *testing.T) {
	f := newFakeClient()
	n := newRecordingNotifier()
	f.waitErr = &rpc.ProtocolError{Err: errors.New("garbage record")}
	c := newTestCoordinator(t, f, n)
	defer c.Close()
	startRunningOperation(t, f, c)

	f.die()
	if err := handleInputUntilFatal(t, c, "second"); err == nil {
		t.Fatal("HandleUserInput during a run after a protocol integrity failure = nil error, want fatal")
	}
	// A dead protocol produces no settlement outcome.
	select {
	case text := <-n.responses:
		t.Errorf("unexpected response %q after the protocol integrity failure", text)
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q after the protocol integrity failure", notice.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}
