package runtime

import (
	"context"
	"time"
)

// The heartbeat makes a silent run visible (design "Heartbeat"): there is no
// watchdog deadline, because no interval can distinguish a hang from a
// legitimate long model call. While an operation runs, the coordinator emits
// a Heartbeat notice every HeartbeatInterval. All methods here are owned by
// the coordinator event loop goroutine.
//
// startHeartbeat arms the ticker for a beginning operation. It does nothing
// when an operation is already running: a steered input joins the ongoing
// run's heartbeat.
func (c *Coordinator) startHeartbeat() {
	if c.op == nil || c.hbTicker != nil {
		return
	}
	c.hbTicker = time.NewTicker(c.cfg.HeartbeatInterval)
	c.hbC = c.hbTicker.C
}

// stopHeartbeat disarms the ticker on every terminal state.
func (c *Coordinator) stopHeartbeat() {
	if c.hbTicker == nil {
		return
	}
	c.hbTicker.Stop()
	c.hbTicker = nil
	c.hbC = nil
}

// heartbeatTick emits one Heartbeat notice: elapsed time, the type and age of
// the last observed event (metadata only, no content), and an abort hint. The
// notice wording is owned by the Telegram design.
func (c *Coordinator) heartbeatTick() {
	if c.op == nil || c.hbTicker == nil {
		return
	}
	c.notifier.Notice(context.Background(), Notice{
		Kind:         NoticeHeartbeat,
		Elapsed:      time.Since(c.op.startedAt),
		LastActivity: c.client.LastActivity(),
	})
}
