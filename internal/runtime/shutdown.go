package runtime

import (
	"context"
	"log/slog"
)

// Shutdown runs the deterministic shutdown sequence (R17, design "Shutdown").
// It posts the shutdown request so the loop enters shutdown mode and reports
// whether an operation is running; an active operation then gets Esc parity
// (clear_queue, then abort) under the deadline; the loop is stopped, pi's
// stdin is closed to request an orderly exit, and the child is waited for
// under the same deadline with a kill fallback. It returns nil in every case:
// shutdown time is never traded for tidiness, and the pod restart is the
// recovery for a child that had to be killed.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	timeout := c.cfg.ShutdownTimeout
	if timeout <= 0 {
		// deliberate: an unset Config falls back to the pinned default rather
		// than validating the whole Config at construction.
		timeout = DefaultConfig().ShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	running, err := c.requestShutdown(ctx)
	if err != nil {
		// The loop already stopped (fatal or closed): no operation state is
		// left to abort, but pi still needs the orderly stdin close below.
		c.log.Info("shutdown request skipped", "error", err)
	}
	if running {
		c.abortForShutdown(ctx)
	}

	// Stop the loop before the child exits: no state transition is left to
	// attribute once shutdown has decided the outcome.
	c.Close()
	if err := c.client.CloseStdin(); err != nil {
		c.log.Error("close pi stdin", "error", err)
	} else {
		c.log.Info("closed pi stdin")
	}
	awaitExit(ctx, c.client, c.log)
	return nil
}

// requestShutdown posts the shutdown request and returns whether an operation
// is running. The loop enters shutdown mode before it acknowledges, so a clean
// pi exit that follows the request is never reported as a fatal coordinator
// error.
func (c *Coordinator) requestShutdown(ctx context.Context) (bool, error) {
	running := make(chan bool, 1)
	if err := c.sendCommand(ctx, coordCommand{kind: cmdShutdown, running: running}); err != nil {
		return false, err
	}
	// The loop sends before it acknowledges, so the buffered value is ready.
	return <-running, nil
}

// abortForShutdown gives the active operation Esc parity: drain queued
// steering and follow-up messages, then abort the run. The abort response is
// the idle point, so no settle is waited for. Failures are logged, never
// fatal: a run that outlives the deadline is a restart's problem, not
// shutdown's.
func (c *Coordinator) abortForShutdown(ctx context.Context) {
	c.log.Info("shutting down during an operation")
	dropped, err := c.clearQueueAndAbort(ctx)
	if err != nil {
		c.log.Error("shutdown abort failed", "error", err)
		return
	}
	// Queue counts are lifecycle metadata; the dropped texts stay out of the
	// log.
	c.log.Info("shutdown aborted the active operation",
		"droppedSteering", len(dropped.Steering),
		"droppedFollowUp", len(dropped.FollowUp))
}

// awaitExit waits for pi to exit under the shutdown deadline and kills the
// child only after it misses the deadline (design "Shutdown"). deliberate: it
// does not join the wait after a kill, because the deadline is the only bound
// shutdown time has; a child that ignored the stdin close must not hold the
// process open past it.
func awaitExit(ctx context.Context, client PiClient, log *slog.Logger) {
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			log.Error("pi exited with failure during shutdown", "error", err)
		} else {
			log.Info("pi exited after the shutdown request")
		}
	case <-ctx.Done():
		log.Warn("pi missed the shutdown deadline; killing the child")
		if err := client.Kill(); err != nil {
			log.Error("kill pi during shutdown", "error", err)
		}
	}
}
