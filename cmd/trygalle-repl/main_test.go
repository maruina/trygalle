package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeHandler records every routed input and fails on demand, simulating an
// active run: settlement is invisible to run(), which keeps routing lines.
type fakeHandler struct {
	mu      sync.Mutex
	inputs  []string
	failOn  string // route returns an error for this text
	failErr error
}

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
	stdin := strings.NewReader("first prompt\nsecond prompt\n/status\n")

	// The first prompt "stays active": the fake settles nothing, while run()
	// keeps accepting lines until EOF.
	if err := run(context.Background(), stdin, nil, h); err != nil {
		t.Fatalf("run with clean EOF: %v", err)
	}
	want := []string{"first prompt", "second prompt", "/status"}
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
	stdin := strings.NewReader("\n  \nhello\n\t\n")
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
	stdin := strings.NewReader("first\nsecond\nthird\n")
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
	err := command([]string{"--help"}, strings.NewReader(""), &stdout, &stderr)
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
	err := command([]string{}, strings.NewReader(""), &stdout, &stderr)
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
		err := command(argv, strings.NewReader(""), &stdout, &stderr)
		if !errors.Is(err, errUsage) {
			t.Fatalf("--heartbeat-interval %s error = %v, want errUsage", value, err)
		}
		if !strings.Contains(stderr.String(), "--heartbeat-interval must be greater than zero") {
			t.Errorf("--heartbeat-interval %s missing diagnostic: %q", value, stderr.String())
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
