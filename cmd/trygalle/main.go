// Command trygalle is the production main process: one long-lived pi
// subprocess through the coordinator, the Telegram transport as its
// Notifier, and the transport-owned poll loop as the user boundary. SIGINT
// and SIGTERM run the shutdown ordering and exit 0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
	"github.com/maruina/trygalle/internal/telegram"
)

var errUsage = errors.New("usage")

const usage = `trygalle runs one pi subprocess in rpc mode, routes Telegram
messages from one allowlisted private chat through the coordinator, and
delivers pi responses and system notices back to that chat.

Usage:
  trygalle [flags]

Flags:
  --pi-bin string             pi binary to run (default "pi")
  --session-dir string        pi session directory (required)
  --no-continue               start pi without --continue (fresh session)
  --heartbeat-interval dur    heartbeat notice interval while a run is active
                              (default 5m0s)
  --shutdown-timeout dur      graceful shutdown deadline (default 30s)
  -h, --help                  show this help

Environment:
  TRYGALLE_TELEGRAM_TOKEN         bot API token (required)
  TRYGALLE_TELEGRAM_ALLOWED_USER  allowlisted numeric user id (required)
  TRYGALLE_TELEGRAM_POLL_TIMEOUT  long-poll timeout (default 30s)

Example:
  trygalle --session-dir /var/lib/trygalle

Notes:
  - SIGINT and SIGTERM stop the poll loop, run the coordinator shutdown
    sequence, and exit 0 within --shutdown-timeout.
  - A Telegram poll conflict (409 or 401) or coordinator death exits 1;
    recovery is operator action, not retry.
`

func main() {
	// SIGINT and SIGTERM end the poll loop; the shutdown ordering that
	// follows is the orderly exit, so the process exits 0 in either case.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	code := 0
	switch err := command(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); {
	case err == nil:
	case errors.Is(err, errUsage):
		// Usage text was already printed by command.
		code = 2
	default:
		fmt.Fprintln(os.Stderr, "trygalle:", err)
		code = 1
	}
	os.Exit(code)
}

// command is the process entry logic; it returns the process exit cause.
// getenv is injected so tests drive the environment without mutating the
// process.
func command(ctx context.Context, argv []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trygalle", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "trygalle: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	if *sessionDir == "" {
		fmt.Fprintln(stderr, "trygalle: --session-dir is required")
		fs.Usage()
		return errUsage
	}
	if *heartbeat <= 0 {
		// runtime.Config.HeartbeatInterval must be positive: it feeds
		// time.NewTicker, which panics on a non-positive duration.
		fmt.Fprintln(stderr, "trygalle: --heartbeat-interval must be greater than zero")
		fs.Usage()
		return errUsage
	}

	token := getenv("TRYGALLE_TELEGRAM_TOKEN")
	if token == "" {
		return errors.New("missing bot token (set TRYGALLE_TELEGRAM_TOKEN)")
	}
	allowedUser, err := strconv.ParseInt(getenv("TRYGALLE_TELEGRAM_ALLOWED_USER"), 10, 64)
	if err != nil {
		return fmt.Errorf("TRYGALLE_TELEGRAM_ALLOWED_USER is not a number: %w", err)
	}
	pollTimeout := time.Duration(0)
	if raw := getenv("TRYGALLE_TELEGRAM_POLL_TIMEOUT"); raw != "" {
		pollTimeout, err = time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("TRYGALLE_TELEGRAM_POLL_TIMEOUT is not a duration: %w", err)
		}
	}

	cfg := runtime.DefaultConfig()
	cfg.PiBin = *piBin
	cfg.Args = buildArgs(*noContinue, *sessionDir)
	cfg.SessionDir = *sessionDir
	cfg.HeartbeatInterval = *heartbeat
	cfg.ShutdownTimeout = *shutdown

	log := slog.New(slog.NewTextHandler(stderr, nil))
	transport, err := telegram.New(telegram.Config{
		Token:       token,
		AllowedUser: allowedUser,
		PollTimeout: pollTimeout,
	}, log)
	if err != nil {
		return fmt.Errorf("telegram transport: %w", err)
	}

	start := func(args []string) (runtime.PiClient, error) {
		return rpc.New(cfg.PiBin, args, rpc.WithLogger(log))
	}
	// The startup validation ladder owns retries and the degraded fresh
	// session (R15); a fatal startup returns here and exits 1.
	client, err := runtime.Start(context.Background(), start, transport, cfg, log)
	if err != nil {
		return fmt.Errorf("pi startup: %w", err)
	}
	coord := runtime.NewCoordinator(client, transport, cfg, log)

	// The poll loop owns the main goroutine: it returns when the signal
	// context ends (the shutdown path) or with a fatal failure (409/401
	// conflict, coordinator death through the handler) the composition
	// exits 1 on.
	if err := transport.RunPoll(ctx, coord); err != nil {
		log.Error("poll loop", "error", err)
		return fmt.Errorf("telegram poll loop: %w", err)
	}

	// Shutdown ordering: the signal stopped the poll loop, the coordinator's
	// bounded sequence aborts an active run and closes pi (its best-effort
	// restart notice enqueues into the live transport), then the
	// transport's bounded Close covers the in-flight send and the
	// single-attempt restart notice before exit 0.
	if err := coord.Shutdown(context.Background()); err != nil {
		log.Error("shutdown", "error", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), *shutdown)
	defer cancel()
	transport.Close(closeCtx)
	return nil
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
