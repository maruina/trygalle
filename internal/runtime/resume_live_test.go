package runtime_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// resumeLiveTimeout bounds every live resume RPC and wait.
const resumeLiveTimeout = 60 * time.Second

// ladderBoot starts one pi child through the runtime startup ladder (R15)
// under the hermetic harness environment and returns the validated client
// plus its notifier, so a test can observe a degraded fresh-session notice.
func ladderBoot(t *testing.T, bin string, args []string) (runtime.PiClient, *recordingNotifier) {
	t.Helper()
	n := newRecordingNotifier()
	cfg := runtime.DefaultConfig()
	cfg.Args = args
	start := func(startArgs []string) (runtime.PiClient, error) {
		return harness.Start(t, bin, harness.WithArgs(startArgs...)), nil
	}
	client, err := runtime.Start(context.Background(), start, n, cfg, testLogger(t))
	if err != nil {
		t.Fatalf("startup ladder: %v", err)
	}
	return client, n
}

// liveState queries get_state on a seam client under the live timeout.
func liveState(t *testing.T, client runtime.PiClient) rpc.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), resumeLiveTimeout)
	defer cancel()
	state, err := client.GetState(ctx)
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	return state
}

// liveStopClient closes the client's stdin and waits bounded for the exit,
// killing on expiry (the same stop shape the ladder and shutdown use).
func liveStopClient(t *testing.T, client *rpc.Client) {
	t.Helper()
	_ = client.CloseStdin()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(resumeLiveTimeout):
		_ = client.Kill()
		<-done
	}
}

// bootSessionWithAnswer creates one session by completing a prompt in a fresh
// session directory, then stops the child. It returns the session id, the
// session file path, and the mock model for later reuse.
func bootSessionWithAnswer(t *testing.T, bin, sessionDir string) (string, string, *harness.MockModel) {
	t.Helper()
	model := harness.StartMockModel(t)
	model.RespondText("resume answer")
	client := harness.Start(t, bin, harness.WithModel(model),
		harness.WithArgs("--mode", "rpc", "--continue", "--session-dir", sessionDir))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))

	before := liveGetState(t, client)
	if before.SessionID == "" {
		t.Fatal("get_state reports no session id on first boot")
	}
	if err := c.HandleUserInput(context.Background(), "create a session"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}
	if text := n.waitForResponse(t); text != "resume answer" {
		t.Fatalf("response = %q, want the scripted answer", text)
	}

	c.Close()
	liveStopClient(t, client)
	return before.SessionID, before.SessionFile, model
}

// TestLiveStartupEmptySessionDir pins resume scenario (a): with --continue on
// an empty session directory, the first boot succeeds and a fresh session id
// appears (design contract test 6a, R15).
func TestLiveStartupEmptySessionDir(t *testing.T) {
	bin := harness.Pi(t)
	sessionDir := t.TempDir()
	client, n := ladderBoot(t, bin, []string{"--mode", "rpc", "--continue", "--session-dir", sessionDir})
	defer liveStopClient(t, client.(*rpc.Client))

	state := liveState(t, client)
	if state.SessionID == "" {
		t.Fatal("get_state reports no session id on an empty session dir")
	}
	select {
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q on a healthy first boot", notice.Kind)
	default:
	}
}

// TestLiveResumeAfterCompletedPrompt pins resume scenario (b): a restart with
// --continue after a completed prompt resumes the same session id (design
// contract test 6b, R15).
func TestLiveResumeAfterCompletedPrompt(t *testing.T) {
	bin := harness.Pi(t)
	sessionDir := t.TempDir()
	id, _, _ := bootSessionWithAnswer(t, bin, sessionDir)

	client, n := ladderBoot(t, bin, []string{"--mode", "rpc", "--continue", "--session-dir", sessionDir})
	defer liveStopClient(t, client.(*rpc.Client))

	after := liveState(t, client)
	if after.SessionID != id {
		t.Fatalf("resumed session id = %q, want %q", after.SessionID, id)
	}
	select {
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q on a healthy resume", notice.Kind)
	default:
	}
}

// TestLiveResumePartialTrailingLine pins resume scenario (c): a latest session
// whose file carries a partial trailing line (the mid-save kill shape) does not
// block startup (design contract test 6c, R15). Pi drops the malformed line and
// resumes the same session.
func TestLiveResumePartialTrailingLine(t *testing.T) {
	bin := harness.Pi(t)
	sessionDir := t.TempDir()
	id, sessionFile, _ := bootSessionWithAnswer(t, bin, sessionDir)

	f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open session file: %v", err)
	}
	// A partial record with no trailing LF, as a mid-save kill would leave.
	if _, err := f.WriteString(`{"type":"message","role":"assistant","content":[{"type":"te`); err != nil {
		t.Fatalf("append partial line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close session file: %v", err)
	}

	client, n := ladderBoot(t, bin, []string{"--mode", "rpc", "--continue", "--session-dir", sessionDir})
	defer liveStopClient(t, client.(*rpc.Client))

	after := liveState(t, client)
	if after.SessionID != id {
		t.Fatalf("session id across a partial trailing line = %q, want %q", after.SessionID, id)
	}
	select {
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q; the damaged session must not degrade the start", notice.Kind)
	default:
	}
}

// TestLiveResumeUnreadableSessionFile pins resume scenario (d): a wholly
// unreadable session file in the directory does not block startup. Pi excludes
// it from discovery and starts a fresh session on the first boot (design
// contract test 6d, R15); the degraded ladder is the alternative bound, so the
// test accepts either path with a session id and records which one ran.
func TestLiveResumeUnreadableSessionFile(t *testing.T) {
	bin := harness.Pi(t)
	sessionDir := t.TempDir()
	name := "2026-01-01T00-00-00-000Z_unreadable.jsonl"
	if err := os.WriteFile(sessionDir+"/"+name, []byte("not readable"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	if err := os.Chmod(sessionDir+"/"+name, 0o000); err != nil {
		t.Fatalf("chmod session file: %v", err)
	}

	client, n := ladderBoot(t, bin, []string{"--mode", "rpc", "--continue", "--session-dir", sessionDir})
	defer liveStopClient(t, client.(*rpc.Client))

	state := liveState(t, client)
	if state.SessionID == "" {
		t.Fatal("get_state reports no session id with an unreadable session file")
	}
	degraded := false
	select {
	case notice := <-n.notices:
		if notice.Kind != runtime.NoticeSessionResumeFailed {
			t.Errorf("unexpected notice %q with an unreadable session file", notice.Kind)
		}
		degraded = true // the ladder bounded the failure to one fresh restart
	default:
	}
	if degraded && state.SessionID == "" {
		t.Fatal("the degraded fresh-session restart reported no session id")
	}
}

// TestLiveKillMidSessionIsFatal pins R16 against the real Pi: a pi process
// killed mid-session surfaces a fatal error through the coordinator, and no
// settlement outcome leaks from the dead run.
func TestLiveKillMidSessionIsFatal(t *testing.T) {
	bin := harness.Pi(t)
	model := harness.StartMockModel(t)
	model.RespondText("never delivered")
	model.Delay(3 * time.Second) // hold the run open past the kill
	client := harness.Start(t, bin, harness.WithModel(model))
	n := newRecordingNotifier()
	c := runtime.NewCoordinator(client, n, runtime.DefaultConfig(), testLogger(t))
	defer c.Close()

	if err := c.HandleUserInput(context.Background(), "run to kill"); err != nil {
		t.Fatalf("HandleUserInput(prompt): %v", err)
	}
	if err := client.Kill(); err != nil {
		t.Fatalf("kill pi mid-session: %v", err)
	}
	if err := handleInputUntilFatal(t, c, "after the kill"); err == nil {
		t.Fatal("HandleUserInput after the mid-session kill = nil error, want fatal")
	}
	if err := c.Wait(); err == nil {
		t.Fatal("coordinator Wait = nil after the mid-session kill, want fatal")
	}
	select {
	case text := <-n.responses:
		t.Errorf("unexpected response %q after the kill", text)
	case notice := <-n.notices:
		t.Errorf("unexpected notice %q after the kill", notice.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}
