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
