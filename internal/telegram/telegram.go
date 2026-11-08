// Package telegram implements a runtime.Notifier for the Telegram Bot API.
//
// The transport owns Telegram adaptation only: outbound rendering, chunking,
// and delivery with bounded retry and hold, plus inbound long polling with
// offset discipline and a numeric allowlist. Reserved-command classification,
// operation state, and completion detection stay in the runtime coordinator.
package telegram

import "time"

const (
	// maxMessageUTF16CodeUnits is Telegram's send limit. Telegram counts it
	// in UTF-16 code units, not runes: astral-plane characters count as two,
	// so 4096 emoji runes still overflow one message and must chunk.
	maxMessageUTF16CodeUnits = 4096

	// maxSendAttempts is the retry budget for one outbound message after a
	// proven-transient failure.
	// deliberate: a fixed, unreviewed budget; five attempts bounded 10s apart
	// covers minute-scale reconnects without a configuration surface. Tuning
	// moves to environment settings on beta evidence (follow-up #5).
	maxSendAttempts = 5

	// sendBackoffCap is the cap on exponential backoff between send retries.
	// deliberate: fixed cap keeping a dead connection from pinning delivery
	// across restarts; revisit with the retry-budget tuning.
	sendBackoffCap = 10 * time.Second

	// maxHeldTerminal is the cap on held terminal messages awaiting a
	// reconnect; the oldest is dropped beyond it.
	// deliberate: a constant, oldest-dropped cap bounds retained memory
	// without a hold deadline; a durable hold queue is the design's rejected
	// branch, so the cap just bounds the worst case.
	maxHeldTerminal = 50

	// pollStallFailures is the consecutive-failure count that logs the
	// polling-stall warning. Detection only; where it alerts is follow-up #5.
	pollStallFailures = 5
)
