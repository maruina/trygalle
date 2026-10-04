package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// TestHelperProcess is not a test to run; it re-runs as the child of the
// client process tests when GO_WANT_HELPER_PROCESS=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "echo":
		if line := os.Getenv("HELPER_STDERR_LINE"); line != "" {
			fmt.Fprintln(os.Stderr, line)
		}
		echoHelper(os.Stdin, os.Stdout)
		// Exit ourselves: the testing framework would print "PASS" to stdout
		// after this test returns, which must never enter the protocol stream.
		os.Exit(0)
	case "read-then-die":
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1024), 1024*1024)
		sc.Scan() // consume one command, then die without responding
		os.Exit(5)
	case "sleep":
		io.Copy(io.Discard, os.Stdin) // never responds; killed by the test
	}
}

// echoHelper answers every stdin command with a success response and exits 0
// on stdin close (an orderly shutdown).
func echoHelper(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	for sc.Scan() {
		var head struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		fmt.Fprintf(w, "{\"id\":%q,\"type\":\"response\",\"command\":%q,\"success\":true}\n", head.ID, head.Type)
	}
}

// startHelperProcess spawns the test binary as a minimal pi stand-in with the
// given helper enverations already in place.
func startHelperProcess(t *testing.T, mode string, extraEnv ...string) (*Client, *mutexBuffer) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	envNames := []string{"GO_WANT_HELPER_PROCESS", "HELPER_MODE", "HELPER_STDERR_LINE"}
	for _, name := range envNames {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
	}
	if err := os.Setenv("GO_WANT_HELPER_PROCESS", "1"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	if err := os.Setenv("HELPER_MODE", mode); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	for _, kv := range extraEnv {
		k, v, _ := bytes.Cut([]byte(kv), []byte("="))
		if err := os.Setenv(string(k), string(v)); err != nil {
			t.Fatalf("setenv %s: %v", k, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range envNames {
			_ = os.Unsetenv(name)
		}
		for _, kv := range extraEnv {
			k, _, _ := bytes.Cut([]byte(kv), []byte("="))
			_ = os.Unsetenv(string(k))
		}
	})
	var stderr mutexBuffer
	client, err := New(exe,
		[]string{"-test.run=^TestHelperProcess$"},
		WithStderr(&stderr),
	)
	if err != nil {
		t.Fatalf("New(helper): %v", err)
	}
	return client, &stderr
}

// TestGracefulStdinClose proves stdin close produces an orderly exit.
func TestGracefulStdinClose(t *testing.T) {
	client, _ := startHelperProcess(t, "echo")
	if err := client.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("Wait after stdin close = %v, want nil", err)
	}
}

// TestPendingSendFailsOnProcessExit proves an outstanding send fails when the
// process dies without responding.
func TestPendingSendFailsOnProcessExit(t *testing.T) {
	client, _ := startHelperProcess(t, "read-then-die")
	_, err := client.Send(context.Background(), GetStateCommand())
	if err == nil {
		t.Fatal("Send succeeded against a dying process, want error")
	}
	if err := client.Wait(); err == nil {
		t.Fatal("Wait = nil, want the exit failure")
	}
}

// TestKillIdempotent proves Kill terminates a stuck child and tolerates
// repeated calls.
func TestKillIdempotent(t *testing.T) {
	client, _ := startHelperProcess(t, "sleep")
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := client.Kill(); err != nil {
		t.Fatalf("second Kill (must be tolerated): %v", err)
	}
	waitErr := client.Wait()
	if waitErr == nil {
		t.Fatal("Wait = nil after Kill, want the kill failure")
	}
}

// TestWaitRepeatedCallers proves every Wait caller observes the same cached
// terminal result.
func TestWaitRepeatedCallers(t *testing.T) {
	client, _ := startHelperProcess(t, "sleep")
	const callers = 4
	results := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = client.Wait()
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let the callers block on Wait
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	wg.Wait()
	for i := 1; i < callers; i++ {
		if results[i] == nil || results[i] != results[0] {
			t.Errorf("caller %d result = %v, want the shared cached result %v", i, results[i], results[0])
		}
	}
}

// TestStderrSeparateFromProtocol proves stderr records never reach the stdout
// protocol parser.
func TestStderrSeparateFromProtocol(t *testing.T) {
	client, stderr := startHelperProcess(t, "echo", "HELPER_STDERR_LINE=diagnostic on stderr")
	// Wait for the child to emit its stderr line before sending.
	deadline := time.Now().Add(2 * time.Second)
	for stderr.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.Send(context.Background(), GetStateCommand())
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if !resp.Success {
		t.Errorf("response = %+v", resp)
	}
	if !bytes.Contains([]byte(stderr.String()), []byte("diagnostic on stderr")) {
		t.Errorf("stderr diagnostics not captured: %q", stderr.String())
	}
	if err := client.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}
