package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// StartFunc starts one Pi child with the given argv and returns its client.
// The startup ladder calls it once per attempt and never overlaps children:
// the previous child has exited before the next call, because at most one Pi
// process may own the session directory (design "Process model and startup").
type StartFunc func(args []string) (PiClient, error)

// Start runs the startup validation ladder (R15): start Pi with cfg.Args and
// validate it under cfg.StartupValidationTimeout — get_state must report a
// session identifier and get_commands a non-empty command list. One failure
// (including the deadline and a child that exited mid-validation) retries once
// with the same argv. A second failure stops that child too, restarts with
// degraded args (no --continue, design "Degraded start"), and reports
// SessionResumeFailed once the fresh session validates; a degraded failure is
// fatal, and the pod restart is the recovery.
func Start(ctx context.Context, start StartFunc, notify Notifier, cfg Config, log *slog.Logger) (PiClient, error) {
	if log == nil {
		log = slog.Default()
	}

	client, err := attempt(ctx, start, cfg, cfg.Args, log)
	if err == nil {
		return client, nil
	}
	log.Error("pi startup validation failed", "error", err)

	// One strike already spent: retry with the same argv for transient causes.
	client, err = attempt(ctx, start, cfg, cfg.Args, log)
	if err == nil {
		return client, nil
	}
	log.Error("pi startup validation failed again", "error", err)

	// Degraded restart: drop --continue so Pi starts a fresh session.
	resumeErr := err // the second strike, the cause the loud notice reports
	client, err = attempt(ctx, start, cfg, cfg.DegradedArgs(), log)
	if err == nil {
		log.Warn("pi started a degraded fresh session after a resume failure")
		notify.Notice(context.Background(), Notice{
			Kind: NoticeSessionResumeFailed,
			Text: resumeErr.Error(),
		})
		return client, nil
	}
	log.Error("degraded pi startup failed", "error", err)
	return nil, fmt.Errorf("pi startup failed after the degraded fresh-session restart: %w", err)
}

// attempt starts one child and validates it. On any failure it stops the child
// before returning, so the next attempt never overlaps a live child on the
// session directory.
func attempt(ctx context.Context, start StartFunc, cfg Config, args []string, log *slog.Logger) (PiClient, error) {
	client, err := start(args)
	if err != nil {
		return nil, fmt.Errorf("start pi: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, cfg.StartupValidationTimeout)
	verr := validateStartup(vctx, client, log)
	cancel() // release a hung validation call before stopping the child
	if verr == nil {
		return client, nil
	}
	stopChild(client, cfg.StartupValidationTimeout, log)
	return nil, verr
}

// validateStartup checks a freshly started child: get_state must report a
// session identifier and get_commands a non-empty command list. Any error,
// including deadline expiry and a child that exited mid-validation, is a
// failure strike for the ladder.
func validateStartup(ctx context.Context, client PiClient, log *slog.Logger) error {
	state, err := client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("get_state: %w", err)
	}
	if state.SessionID == "" {
		return errors.New("get_state reported no session identifier")
	}
	commands, err := client.GetCommands(ctx)
	if err != nil {
		return fmt.Errorf("get_commands: %w", err)
	}
	if len(commands) == 0 {
		return errors.New("get_commands reported no commands")
	}
	// The resumed session identifier and the per-source command counts are
	// lifecycle metadata; command names are never logged.
	log.Info("pi startup validated",
		"sessionID", state.SessionID,
		"commandsBySource", countCommandsBySource(commands))
	return nil
}

// countCommandsBySource counts Pi's registered commands by source category
// (extension, prompt, skill) for the startup log record.
func countCommandsBySource(commands []CommandInfo) map[string]int {
	counts := make(map[string]int, 3)
	for _, cmd := range commands {
		counts[cmd.Source]++
	}
	return counts
}

// stopChild ends one started child: close stdin, wait under the bounded
// deadline, and kill on expiry. After a kill it joins the exit so the next
// start never overlaps a child on the session directory.
// deliberate: the stop bound reuses StartupValidationTimeout; the plan pins no
// separate constant, and a stdin close ends a healthy child promptly.
func stopChild(client PiClient, deadline time.Duration, log *slog.Logger) {
	_ = client.CloseStdin()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if err := client.Kill(); err != nil {
			log.Error("kill pi child", "error", err)
		}
		// A killed child cannot refuse to exit; joining here keeps the
		// one-child-per-session-directory invariant before the next start.
		<-done
	}
}
