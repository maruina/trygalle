// Command trygalle-repl is the dev-only demo surface for the trygalle
// coordinator: it routes stdin lines through the coordinator to one long-lived
// pi rpc subprocess. It is not the production container main process; the
// reserved commands /abort and /new route later in slice 4 (Tasks 14-15), and SIGINT and
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
const (
	maxLineSize          = 1 << 20
	scannerInitialBuffer = 64 * 1024
)

// inputHandler is the part of the runtime coordinator the REPL loop needs.
// The real *runtime.Coordinator satisfies it.
type inputHandler interface {
	HandleUserInput(ctx context.Context, text string) error
	Done() <-chan struct{}
	Wait() error
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
  --shutdown-timeout dur      graceful shutdown deadline (default 30s)
  -h, --help                  show this help

Example:
  trygalle-repl --session-dir /tmp/trygalle-demo

Notes:
  - Reserved commands /abort and /new land later in slice 4 (Tasks 14-15).
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
func command(argv []string, stdin io.ReadCloser, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trygalle-repl", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stdout, usage) }
	piBin := fs.String("pi-bin", "pi", "pi binary to run")
	sessionDir := fs.String("session-dir", "", "pi session directory (required)")
	noContinue := fs.Bool("no-continue", false, "start pi without --continue (fresh session)")
	heartbeat := fs.Duration("heartbeat-interval", 5*time.Minute, "heartbeat notice interval while a run is active")
	shutdown := fs.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown deadline")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
	if *heartbeat <= 0 {
		// runtime.Config.HeartbeatInterval must be positive: it feeds
		// time.NewTicker, which panics on a non-positive duration.
		fmt.Fprintln(stderr, "trygalle-repl: --heartbeat-interval must be greater than zero")
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
func run(ctx context.Context, stdin io.ReadCloser, stdout io.Writer, handler inputHandler) error {
	stop := make(chan struct{})
	lines, readErr, readerDone := scanInput(stdin, stop)
	defer stopInputReader(stop, stdin, readerDone)
	return routeInput(ctx, lines, readErr, handler)
}

func scanInput(stdin io.Reader, stop <-chan struct{}) (<-chan string, <-chan error, <-chan struct{}) {
	lines := make(chan string)
	readErr := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(lines)
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 0, scannerInitialBuffer), maxLineSize)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-stop:
				readErr <- nil
				return
			}
		}
		readErr <- scanner.Err()
	}()
	return lines, readErr, readerDone
}

func stopInputReader(stop chan struct{}, stdin io.ReadCloser, readerDone <-chan struct{}) {
	close(stop)
	if inputIsProcessStdin(stdin) {
		return
	}
	_ = stdin.Close()
	<-readerDone
}

func inputIsProcessStdin(stdin io.ReadCloser) bool {
	file, ok := stdin.(*os.File)
	return ok && file == os.Stdin
}

func routeInput(ctx context.Context, lines <-chan string, readErr <-chan error, handler inputHandler) error {
	for {
		select {
		case <-handler.Done():
			return handler.Wait()
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return finishInput(handler, readErr)
			}
			if err := handleInputLine(ctx, handler, line); err != nil {
				return err
			}
		}
	}
}

func finishInput(handler inputHandler, readErr <-chan error) error {
	select {
	case <-handler.Done():
		return handler.Wait()
	default:
	}
	if err := <-readErr; err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

func handleInputLine(ctx context.Context, handler inputHandler, line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	return handler.HandleUserInput(ctx, line)
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
	case runtime.NoticeStatus:
		if n.State == nil {
			return "status: no state reported"
		}
		// State carries session metadata only; no prompt or tool content.
		text = fmt.Sprintf("session %s; streaming=%t compacting=%t pending=%d messages=%d",
			n.State.SessionID, n.State.IsStreaming, n.State.IsCompacting,
			n.State.PendingMessageCount, n.State.MessageCount)
		if n.State.Model != nil {
			text += "; model " + n.State.Model.ID
		}
		return text
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
	case runtime.NoticeExtensionHandled:
		text = "extension command handled; the active run continues"
	case runtime.NoticeFailedRun:
		text = "run failed: " + n.Text
	case runtime.NoticeRoutingFailure:
		text = "could not deliver the message: " + n.Text
	case runtime.NoticeAbortReport:
		text = "aborted the run"
		if n.Text != "" {
			text += "; " + n.Text
		}
		// The dropped queue texts prove nothing survives the abort (R12).
		if len(n.Dropped.Steering) > 0 {
			text += fmt.Sprintf("; dropped steering: %q", n.Dropped.Steering)
		}
		if len(n.Dropped.FollowUp) > 0 {
			text += fmt.Sprintf("; dropped follow-up: %q", n.Dropped.FollowUp)
		}
		return text
	default:
		text = string(n.Kind)
	}
	return text
}
