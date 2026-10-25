package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/runtime"
)

// fakeHandler records every routed input and fails on demand, simulating an
// active run: settlement is invisible to run(), which keeps routing lines.
type fakeHandler struct {
	mu      sync.Mutex
	inputs  []string
	failOn  string // route returns an error for this text
	failErr error
	done    chan struct{}
}

func (h *fakeHandler) Done() <-chan struct{} { return h.done }
func (h *fakeHandler) Wait() error           { return h.failErr }

func (h *fakeHandler) HandleUserInput(ctx context.Context, text string) error {
	h.mu.Lock()
	h.inputs = append(h.inputs, text)
	h.mu.Unlock()
	if text == h.failOn {
		return h.failErr
	}
	return nil
}

func (h *fakeHandler) routed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.inputs...)
}

func TestRunRoutesInputsBeforeSettlementAndExitsOnEOF(t *testing.T) {
	h := &fakeHandler{}
	stdin := io.NopCloser(strings.NewReader("first prompt\nsecond prompt\n/status\n/abort\n/new\n"))

	// The first prompt "stays active": the fake settles nothing, while run()
	// keeps accepting lines until EOF.
	if err := run(context.Background(), stdin, nil, h); err != nil {
		t.Fatalf("run with clean EOF: %v", err)
	}
	want := []string{"first prompt", "second prompt", "/status", "/abort", "/new"}
	got := h.routed()
	if len(got) != len(want) {
		t.Fatalf("routed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("routed[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunSkipsBlankLines(t *testing.T) {
	h := &fakeHandler{}
	stdin := io.NopCloser(strings.NewReader("\n  \nhello\n\t\n"))
	if err := run(context.Background(), stdin, nil, h); err != nil {
		t.Fatalf("run: %v", err)
	}
	if routed := h.routed(); len(routed) != 1 || routed[0] != "hello" {
		t.Errorf("routed = %v, want [hello]", routed)
	}
}

func TestRunReturnsHandlerError(t *testing.T) {
	wantErr := errors.New("coordinator fatal")
	h := &fakeHandler{failOn: "second", failErr: wantErr}
	stdin := io.NopCloser(strings.NewReader("first\nsecond\nthird\n"))
	if err := run(context.Background(), stdin, nil, h); !errors.Is(err, wantErr) {
		t.Errorf("run error = %v, want %v", err, wantErr)
	}
	// The fatal line never routed the input after it.
	routed := h.routed()
	if len(routed) != 2 || routed[1] != "second" {
		t.Errorf("routed = %v, want to stop at the failing line", routed)
	}
}

func TestCommandHelpExitsCleanly(t *testing.T) {
	var stdout, stderr strings.Builder
	err := command(context.Background(), []string{"--help"}, io.NopCloser(strings.NewReader("")), &stdout, &stderr)
	if err != nil {
		t.Fatalf("command(--help) error = %v, want a clean exit", err)
	}
	if !strings.Contains(stdout.String(), "--session-dir") || !strings.Contains(stdout.String(), "Example:") {
		t.Errorf("usage missing on stdout:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Errorf("help printed to stderr: %q", stderr.String())
	}
}

func TestCommandRequiresSessionDir(t *testing.T) {
	var stdout, stderr strings.Builder
	err := command(context.Background(), []string{}, io.NopCloser(strings.NewReader("")), &stdout, &stderr)
	if !errors.Is(err, errUsage) {
		t.Fatalf("command() error = %v, want errUsage", err)
	}
	if !strings.Contains(stderr.String(), "--session-dir is required") {
		t.Errorf("missing required-flag diagnostic on stderr: %q", stderr.String())
	}
}

func TestCommandRejectsNonPositiveHeartbeat(t *testing.T) {
	for _, value := range []string{"0", "-1s"} {
		var stdout, stderr strings.Builder
		argv := []string{"--session-dir", "/tmp/s", "--heartbeat-interval", value}
		err := command(context.Background(), argv, io.NopCloser(strings.NewReader("")), &stdout, &stderr)
		if !errors.Is(err, errUsage) {
			t.Fatalf("--heartbeat-interval %s error = %v, want errUsage", value, err)
		}
		if !strings.Contains(stderr.String(), "--heartbeat-interval must be greater than zero") {
			t.Errorf("--heartbeat-interval %s missing diagnostic: %q", value, stderr.String())
		}
	}
}

func TestRunDoesNotCloseProcessStdin(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = reader
	defer func() {
		os.Stdin = originalStdin
		_ = reader.Close()
		_ = writer.Close()
	}()

	h := &fakeHandler{done: make(chan struct{})}
	close(h.done)
	if err := run(context.Background(), reader, nil, h); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if _, err := reader.Stat(); err != nil {
		t.Fatalf("process stdin was closed: %v", err)
	}
}

func TestRunReturnsFatalWhileStdinOpen(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	want := errors.New("pi failed")
	h := &fakeHandler{done: make(chan struct{}), failErr: want}
	result := make(chan error, 1)
	go func() { result <- run(context.Background(), reader, nil, h) }()
	close(h.done)
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("run = %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		reader.Close()
		t.Fatal("run waited for stdin after failure")
	}
}

func TestRenderNoticeAbortReport(t *testing.T) {
	got := renderNotice(runtime.Notice{
		Kind:    runtime.NoticeAbortReport,
		Dropped: runtime.QueueContents{Steering: []string{"steered"}, FollowUp: []string{"follow"}},
	})
	for _, want := range []string{"aborted", `"steered"`, `"follow"`} {
		if !strings.Contains(got, want) {
			t.Errorf("renderNotice(AbortReport) = %q, want it to contain %q", got, want)
		}
	}

	idle := renderNotice(runtime.Notice{Kind: runtime.NoticeAbortReport, Text: "no operation was active"})
	if !strings.Contains(idle, "no operation was active") {
		t.Errorf("renderNotice(idle AbortReport) = %q, want the no-operation marker", idle)
	}
}

func TestRenderNoticeStatus(t *testing.T) {
	got := renderNotice(runtime.Notice{
		Kind:  runtime.NoticeStatus,
		State: &runtime.State{SessionID: "sess-1", IsStreaming: true, MessageCount: 2},
	})
	for _, want := range []string{"sess-1", "streaming=true", "messages=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderNotice(Status) = %q, want it to contain %q", got, want)
		}
	}
}

func TestRenderNoticeNewSessionReport(t *testing.T) {
	got := renderNotice(runtime.Notice{
		Kind:    runtime.NoticeNewSessionReport,
		Dropped: runtime.QueueContents{Steering: []string{"steered"}},
	})
	for _, want := range []string{"new session", `"steered"`} {
		if !strings.Contains(got, want) {
			t.Errorf("renderNotice(NewSessionReport) = %q, want it to contain %q", got, want)
		}
	}

	cancelled := renderNotice(runtime.Notice{Kind: runtime.NoticeNewSessionReport, Text: "session switch cancelled"})
	if !strings.Contains(cancelled, "session switch cancelled") {
		t.Errorf("renderNotice(cancelled NewSessionReport) = %q, want the cancellation marker", cancelled)
	}
}

func TestRenderNoticeSessionResumeFailed(t *testing.T) {
	got := renderNotice(runtime.Notice{
		Kind: runtime.NoticeSessionResumeFailed,
		Text: "get_state: boom",
	})
	for _, want := range []string{"session resume failed", "fresh conversation", "get_state: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderNotice(SessionResumeFailed) = %q, want it to contain %q", got, want)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	if got := buildArgs(false, "/data/s"); strings.Join(got, " ") != "--mode rpc --continue --session-dir /data/s" {
		t.Errorf("buildArgs(false) = %v", got)
	}
	if got := buildArgs(true, "/data/s"); strings.Join(got, " ") != "--mode rpc --session-dir /data/s" {
		t.Errorf("buildArgs(true) = %v", got)
	}
}

func TestRenderNoticeRestartNotice(t *testing.T) {
	got := renderNotice(runtime.Notice{Kind: runtime.NoticeRestartNotice})
	for _, want := range []string{"restarting", "closing the pi session"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderNotice(RestartNotice) = %q, want it to contain %q", got, want)
		}
	}
}

func TestExitCauseDropsSignalCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exitCause(canceled, context.Canceled); err != nil {
		t.Errorf("exitCause after a signal = %v, want a clean exit", err)
	}

	live := context.Background()
	if err := exitCause(live, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("exitCause without a signal = %v, want the cancellation", err)
	}

	fatal := errors.New("pi failed")
	if err := exitCause(canceled, fatal); !errors.Is(err, fatal) {
		t.Errorf("exitCause(fatal) = %v, want the fatal error", err)
	}
}
