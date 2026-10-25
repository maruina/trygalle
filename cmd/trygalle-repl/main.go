// Command trygalle-repl is the dev-only demo surface for the trygalle
// coordinator: it routes stdin lines through the coordinator to one long-lived
// pi rpc subprocess. It is not the production container main process; SIGINT
// and SIGTERM run the runtime shutdown sequence and exit 0.
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
	"os/signal"
	"strings"
	"syscall"
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
  - SIGINT and SIGTERM abort an active run, close pi's stdin, and exit 0
    within --shutdown-timeout.
  - EOF (Ctrl-D) exits cleanly.
`

func main() {
	// SIGINT and SIGTERM end the input loop; Shutdown then bounds the exit, so
	// the process exits 0 in either case (R17).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	code := 0
	switch err := command(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); {
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

// command is the process entry logic; it returns the process exit cause. The
// context carries the signal cancellation, so a SIGTERM unwinds the same path
// as a fatal runtime error and still exits 0.
func command(ctx context.Context, argv []string, stdin io.ReadCloser, stdout, stderr io.Writer) error {
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
	start := func(args []string) (runtime.PiClient, error) {
		return rpc.New(cfg.PiBin, args, rpc.WithLogger(log))
	}
	// The startup validation ladder owns retries and the degraded fresh
	// session (R15); a fatal startup returns here and exits 1.
	client, err := runtime.Start(context.Background(), start, stdoutNotifier{w: stdout}, cfg, log)
	if err != nil {
		return fmt.Errorf("pi startup: %w", err)
	}
	coord := runtime.NewCoordinator(client, stdoutNotifier{w: stdout}, cfg, log)

	runErr := exitCause(ctx, run(ctx, stdin, stdout, coord))
	// Shutdown owns the ordered close: abort an active run with Esc parity,
	// close pi's stdin, and bound its exit (R17). It returns nil either way.
	if err := coord.Shutdown(context.Background()); err != nil {
		log.Error("shutdown", "error", err)
	}

	return runErr
}

// exitCause drops the signal cancellation from the exit cause: a SIGINT or
// SIGTERM only ends the input loop, and the shutdown sequence that follows is
// the orderly exit, so the process exits 0 (R17, R18). Every other failure is
// still returned.
func exitCause(ctx context.Context, runErr error) error {
	if errors.Is(runErr, context.Canceled) && ctx.Err() != nil {
		return nil
	}
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
	case runtime.NoticeNewSessionReport:
		text = "started a new session"
		if n.Text != "" {
			text += "; " + n.Text
		}
		// The dropped queue texts prove nothing survives the switch (R13).
		if len(n.Dropped.Steering) > 0 {
			text += fmt.Sprintf("; dropped steering: %q", n.Dropped.Steering)
		}
		if len(n.Dropped.FollowUp) > 0 {
			text += fmt.Sprintf("; dropped follow-up: %q", n.Dropped.FollowUp)
		}
		return text
	case runtime.NoticeSessionResumeFailed:
		text = "session resume failed; started a fresh conversation: " + n.Text
	case runtime.NoticeRestartNotice:
		// Best-effort and non-blocking: shutdown ends the process regardless
		// of whether this notice is delivered.
		text = "restarting; closing the pi session"
	default:
		text = string(n.Kind)
	}
	return text
}
