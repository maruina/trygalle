package runtime

import (
	"context"
	"fmt"
)

func (c *Coordinator) handleResult(cmd coordCommand) {
	if c.pending == nil || c.pending != cmd.op {
		cmd.ack <- errNoPendingInput
		return
	}
	pending := c.pending
	defer func() { c.pending = nil }()
	if cmd.err != nil {
		c.handlePromptFailure(cmd)
		return
	}
	switch cmd.disposition {
	case DispositionStarted:
		c.handleStarted(pending, cmd)
	case DispositionQueued:
		c.handleQueued(cmd)
	case DispositionHandled:
		c.handleHandled(pending, cmd)
	default:
		c.handleUnexpectedDisposition(cmd)
	}
}

func (c *Coordinator) handlePromptFailure(cmd coordCommand) {
	// A submission that failed to reach Pi is a routing failure; the input is
	// never dropped silently (design "Message routing").
	c.log.Error("prompt failed", "error", cmd.err)
	c.notifier.Notice(context.Background(), Notice{Kind: NoticeRoutingFailure, Text: cmd.err.Error()})
	cmd.ack <- nil
}

// handleAbort reports one /abort sequence outcome as a loop-owned notice.
// A failed sequence is a routing failure and the operation (if any) keeps
// running so its settle is still attributed.
//
// The reserved-command window decides attribution: an operation that settled
// while the sequence ran was deferred, so the AbortReport is the operation's
// single terminal outcome. The report names the no-operation case only when no
// operation was active when /abort opened the window. The window suppresses
// trailing events until the next agent_start, so a late settle from the
// aborted run can never complete a later input.
func (c *Coordinator) handleAbort(cmd coordCommand) {
	hadOp := c.reservedHadOp
	c.reserved = false
	c.reservedHadOp = false
	if cmd.err != nil {
		c.flushDeferred()
		c.log.Error("abort sequence failed", "error", cmd.err)
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeRoutingFailure, Text: cmd.err.Error()})
		cmd.ack <- nil
		return
	}
	c.discardDeferred()
	if !hadOp {
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeAbortReport, Text: "no operation was active", Dropped: cmd.dropped})
	} else {
		c.op = nil
		c.state = stateSuperseded
		c.stopHeartbeat()
		c.log.Info("operation aborted")
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeAbortReport, Dropped: cmd.dropped})
	}
	cmd.ack <- nil
}

// handleNew reports one /new sequence outcome as a loop-owned notice. A
// failed sequence is a routing failure and the operation (if any) keeps
// running so its settle is still attributed. A cancelled switch keeps the
// current session (and its active run); the queue was still cleared, so the
// report carries both, and an operation that settled inside the window is
// flushed so its answer is not lost. A completed switch supersedes the
// running operation: the report is its single terminal outcome (any deferred
// one is dropped) and trailing old-run events stay suppressed until the next
// agent_start (see handleEvent).
func (c *Coordinator) handleNew(cmd coordCommand) {
	c.reserved = false
	c.reservedHadOp = false
	if cmd.err != nil {
		c.flushDeferred()
		c.log.Error("new session sequence failed", "error", cmd.err)
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeRoutingFailure, Text: cmd.err.Error()})
		cmd.ack <- nil
		return
	}
	if cmd.cancelled {
		c.flushDeferred()
		c.log.Info("new session cancelled; keeping current session")
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeNewSessionReport, Text: "session switch cancelled", Dropped: cmd.dropped})
		cmd.ack <- nil
		return
	}
	c.discardDeferred()
	c.op = nil
	c.state = stateSuperseded
	c.stopHeartbeat()
	c.log.Info("session superseded")
	c.notifier.Notice(context.Background(), Notice{Kind: NoticeNewSessionReport, Dropped: cmd.dropped})
	cmd.ack <- nil
}

// handleStatus reports one /status query outcome as a loop-owned notice. A
// failed get_state is a routing failure with the failure cause, mirroring
// the prompt-failure pattern; neither path is silent. The query never touches
// operation state: status observes the run without disturbing it (R11).
func (c *Coordinator) handleStatus(cmd coordCommand) {
	if cmd.err != nil {
		c.log.Error("get_state failed", "error", cmd.err)
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeRoutingFailure, Text: cmd.err.Error()})
	} else {
		state := cmd.status
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeStatus, State: &state})
	}
	cmd.ack <- nil
}

func (c *Coordinator) handleStarted(pending *operation, cmd coordCommand) {
	if !c.startOperation(pending) {
		cmd.ack <- nil
		return
	}
	if pending.settled {
		// Fast completion: the run settled before the prompt response was processed.
		c.settleCurrentOperation()
	}
	cmd.ack <- nil
}

func (c *Coordinator) handleQueued(cmd coordCommand) {
	// The input steered into the active run; acceptance only. The run's
	// operation completes at agent_settled, and the steered text is part
	// of that run's single final response.
	c.notifier.Notice(context.Background(), Notice{Kind: NoticeSteerAcknowledged})
	cmd.ack <- nil
}

func (c *Coordinator) handleUnexpectedDisposition(cmd coordCommand) {
	// A disposition outside the pinned set means protocol drift; the input is
	// never dropped silently (design "Message routing").
	c.log.Error("unexpected disposition", "disposition", cmd.disposition)
	c.notifier.Notice(context.Background(), Notice{
		Kind: NoticeRoutingFailure,
		Text: fmt.Sprintf("unexpected disposition %q", cmd.disposition),
	})
	cmd.ack <- fmt.Errorf("unexpected disposition %q", cmd.disposition)
}

// handleHandled attributes a handled input (design "Operation state machine"):
// an extension handler consumed the prompt, and through pi.sendMessage it may
// have synchronously started its own run. For that pinned Pi path, agent_start
// for the run arrives before the handled response on the wire.
func (c *Coordinator) handleHandled(pending *operation, cmd coordCommand) {
	c.drainEvents()
	switch {
	case c.op != nil:
		// An extension command handled mid-run: it executed, no run started
		// for it, and the active run continues, so the input gets its own
		// outcome instead of silence. deliberate: the wording is a placeholder
		// until the Telegram design owns notice text.
		c.log.Info("handled input while an operation is running")
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeExtensionHandled})
	case pending.hasRun:
		// The extension run's agent_start was observed (or already drained)
		// before the response: track the run to its own agent_settled.
		c.startOperation(pending)
		if pending.settled {
			c.settleCurrentOperation()
		}
	default:
		// Stale-text guard: no run belongs to this input, so the last
		// assistant text could only be the previous run's. Complete the
		// input immediately without querying Pi.
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeEmptyResponse})
	}
	cmd.ack <- nil
}
