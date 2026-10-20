// Command trygalle-repl is the dev-only demo surface for the trygalle
// coordinator: it routes stdin lines through the coordinator to one long-lived
// pi rpc subprocess. It is not the production container main process; the
// reserved commands (/status, /abort, /new) route in slice 4, and SIGINT and
// SIGTERM wiring lands with the shutdown task.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// maxLineSize bounds one stdin line. deliberate: the framing limit rule
// applies to the protocol path; stdin lines belong to the demo surface, and
// 1 MiB comfortably exceeds any interactive prompt.
const maxLineSize = 1 << 20

// inputHandler is the part of the runtime coordinator the REPL loop needs.
// The real *runtime.Coordinator satisfies it.
type inputHandler interface {
	HandleUserInput(ctx context.Context, text string) error
}

var errUsage = errors.New("usage")

const usage = `trygalle-repl runs one pi subprocess in rpc mode and routes stdin
lines through the trygalle coordinator, printing pi responses and trygalle
system notices on stdout.

Usage:
  trygalle-repl --session-dir <dir> [flags]

Flags:
  --pi-bin string             pi binary to run (default "pi")
  --session-dir string        pi session directory (required)
  --no-continue               start pi without --continue (fresh session)
  --heartbeat-interval dur    heartbeat notice interval while a run is active
                              (default 5m0s)
  --shutdown-timeout dur      graceful shutdown deadline (default 30s0s)
  -h, --help                  show this help

Example:
  trygalle-repl --session-dir /tmp/trygalle-demo

Notes:
  - Reserved commands /status, /abort, and /new land in slice 4.
  - SIGINT and SIGTERM handling lands with the shutdown task; EOF (Ctrl-D)
    exits cleanly.
`

func main() {
	code := 0
	switch err := command(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); {
	case err == nil:
	case errors.Is(err, errUsage):
		// Usage text was already printed by command.
		code = 2
	default:
		fmt.Fprintln(os.Stderr, "trygalle-repl:", err)
		code = 1
	}
	os.Exit(code)
}

// command is the process entry logic; it returns the process exit cause.
func command(argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trygalle-repl", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stdout, usage) }
	piBin := fs.String("pi-bin", "pi", "pi binary to run")
	sessionDir := fs.String("session-dir", "", "pi session directory (required)")
	noContinue := fs.Bool("no-continue", false, "start pi without --continue (fresh session)")
	heartbeat := fs.Duration("heartbeat-interval", 5*time.Minute, "heartbeat notice interval while a run is active")
	shutdown := fs.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown deadline")
	if err := fs.Parse(argv); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "trygalle-repl: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	if *sessionDir == "" {
		fmt.Fprintln(stderr, "trygalle-repl: --session-dir is required")
		fs.Usage()
		return errUsage
	}

	cfg := runtime.DefaultConfig()
	cfg.PiBin = *piBin
	cfg.Args = buildArgs(*noContinue, *sessionDir)
	cfg.SessionDir = *sessionDir
	cfg.HeartbeatInterval = *heartbeat
	cfg.ShutdownTimeout = *shutdown

	log := slog.New(slog.NewTextHandler(stderr, nil))
	client, err := rpc.New(cfg.PiBin, cfg.Args, rpc.WithLogger(log))
	if err != nil {
		return fmt.Errorf("start pi: %w", err)
	}
	coord := runtime.NewCoordinator(client, stdoutNotifier{w: stdout}, cfg, log)

	runErr := run(context.Background(), stdin, stdout, coord)

	// minimal shutdown for this slice (full clear_queue -> abort ladder and
	// signal wiring land with the shutdown task): stop the coordinator loop,
	// then ask pi for an orderly exit and bound the wait.
	coord.Close()
	if err := client.CloseStdin(); err != nil {
		log.Error("close pi stdin", "error", err)
	}
	waitPiExit(client, cfg.ShutdownTimeout, log)

	return runErr
}

// buildArgs composes the startup command line: rpc mode, the latest-session
// resume unless disabled, and the session directory.
func buildArgs(noContinue bool, sessionDir string) []string {
	args := []string{"--mode", "rpc"}
	if !noContinue {
		args = append(args, "--continue")
	}
	return append(args, "--session-dir", sessionDir)
}

// run reads lines from stdin and routes each to the coordinator. It returns
// on clean EOF (nil) or with the coordinator's fatal error. Because
// HandleUserInput returns after the RPC response, this loop accepts further
// lines while a run is active.
func run(ctx context.Context, stdin io.Reader, stdout io.Writer, handler inputHandler) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := handler.HandleUserInput(ctx, line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// waitPiExit waits for pi's orderly exit up to the shutdown deadline, killing
// the child only after it misses the deadline (design "Shutdown").
func waitPiExit(client *rpc.Client, timeout time.Duration, log *slog.Logger) {
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			log.Error("pi exited with failure", "error", err)
		}
	case <-timer.C:
		if err := client.Kill(); err != nil {
			log.Error("kill pi", "error", err)
		}
	}
}

// stdoutNotifier implements runtime.Notifier on stdout: responses print as-is,
// notices carry a [trygalle] prefix so pi output and system notices are easy
// to tell apart. The wording is owned by the Telegram design; these renderings
// serve the demo surface only.
type stdoutNotifier struct {
	w io.Writer
}

func (n stdoutNotifier) Response(ctx context.Context, text string) {
	fmt.Fprintln(n.w, text)
}

func (n stdoutNotifier) Notice(ctx context.Context, notice runtime.Notice) {
	fmt.Fprintf(n.w, "[trygalle] %s\n", renderNotice(notice))
}

func renderNotice(n runtime.Notice) string {
	text := ""
	switch n.Kind {
	case runtime.NoticeHeartbeat:
		if n.LastActivity.Type == "" {
			return fmt.Sprintf("running for %s; no pi activity yet; send /abort to stop", n.Elapsed)
		}
		return fmt.Sprintf("running for %s; last pi event %s %s ago; send /abort to stop",
			n.Elapsed, n.LastActivity.Type, time.Since(n.LastActivity.At).Round(time.Second))
	case runtime.NoticeSteerAcknowledged:
		text = "queued the message into the active run"
	case runtime.NoticeEmptyResponse:
		text = "pi returned no text"
	case runtime.NoticeFailedRun:
		text = "run failed: " + n.Text
	case runtime.NoticeRoutingFailure:
		text = "could not deliver the message: " + n.Text
	default:
		text = string(n.Kind)
	}
	return text
}
