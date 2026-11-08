package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
)

// qmsg is one outbound chunk: a rendered, size-bounded message with its
// delivery class and retry state.
type qmsg struct {
	class string // log metadata: "response" or the runtime.NoticeKind string
	text  string
	units int

	once      bool // single attempt, never held: the restart notice drain
	supersede bool // recurring kind: a newer pending replaces older ones

	group    uint64 // render batch; a supersede never consumes its own batch
	attempts int
	dead     bool // superseded while undelivered: dropped, never retried
}

// enqueueBatch renders once per class and enqueues one message per chunk.
func (t *Transport) enqueueBatch(class, text string, once, supersede bool) {
	group := t.newGroup()
	for _, part := range chunk(text) {
		t.enqueue(group, &qmsg{
			class:     class,
			text:      part,
			units:     utf16StringLen(part),
			once:      once,
			supersede: supersede,
		})
	}
}

// newGroup mints a batch identity so an interleaved supersede cannot remove
// a later chunk of its own batch.
func (t *Transport) newGroup() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextGroup++
	return t.nextGroup
}

// enqueue joins the hold queue: capped for terminal messages, superseded for
// recurring ones, safe-dropped after close. Non-blocking, so the coordinator
// event loop never waits.
func (t *Transport) enqueue(group uint64, msg *qmsg) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return // safe drop after close
	}
	msg.group = group
	if msg.supersede {
		// A recurring message that is undelivered is replaceable: done for
		// older pending chunks and for the in-flight head. Its own batch
		// stays; chunks accumulate until the batch is complete.
		if t.current != nil && t.current.class == msg.class && t.current.group != msg.group {
			t.current.dead = true
		}
		kept := t.queue[:0]
		for _, q := range t.queue {
			if q.class != msg.class || q.group == msg.group {
				kept = append(kept, q)
			}
		}
		t.queue = kept
	}
	t.queue = append(t.queue, msg)
	held := len(t.queue)
	if t.current != nil {
		held++
	}
	// deliberate: the in-flight head is exempt from eviction because an
	// attempt is already on the wire; killing it mid-send buys nothing.
	for held > maxHeldTerminal && len(t.queue) > 0 {
		evicted := t.queue[0]
		t.queue = t.queue[1:]
		held--
		t.log.Warn("telegram held message evicted (cap)",
			"class", evicted.class, "units", evicted.units)
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// run is the delivery goroutine: the single owner of send attempts.
func (t *Transport) run() {
	defer close(t.done)
	for {
		msg := t.claim()
		if msg == nil {
			select {
			case <-t.wake:
			case <-t.stop:
				return
			}
			continue
		}
		t.deliver(msg)
	}
}

// claim pops the queue head. While stopped, held messages are discarded
// (lost with the process, design-accepted); only once-messages survive.
func (t *Transport) claim() *qmsg {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		kept := t.queue[:0]
		discarded := 0
		for _, q := range t.queue {
			if q.once {
				kept = append(kept, q)
			} else {
				discarded++
			}
		}
		t.queue = kept
		if discarded > 0 {
			t.log.Warn("telegram held messages discarded at shutdown", "count", discarded)
		}
	}
	if len(t.queue) == 0 {
		return nil
	}
	msg := t.queue[0]
	t.queue = t.queue[1:]
	t.current = msg
	return msg
}

// drop releases the current in-flight message without cost to the queue.
func (t *Transport) drop() {
	t.mu.Lock()
	t.current = nil
	t.mu.Unlock()
}

// deliver sends one message through the retry rules: transient failures
// back off while the budget remains, flood waits respect retry_after
// without the budget, ambiguity never resends, permanence never retries.
func (t *Transport) deliver(msg *qmsg) {
	for {
		t.mu.Lock()
		aborted := msg.dead || (t.stopped && !msg.once)
		t.mu.Unlock()
		if aborted {
			// The superseded status of a closed hold: dropped silently,
			// never retried — recovery here would resend a superseded
			// message.
			t.log.Debug("telegram message dropped without send", "class", msg.class,
				"reason", "superseded")
			t.drop()
			return
		}

		params := &bot.SendMessageParams{ChatID: t.cfg.AllowedUser, Text: msg.text}
		_, err := t.sender.SendMessage(t.rectx, params)
		if err == nil {
			t.drop()
			return
		}
		outcome, retryAfter := classifySendError(err)

		switch outcome {
		case outcomeRetry:
			t.mu.Lock()
			msg.attempts++
			t.mu.Unlock()
			if msg.attempts >= maxSendAttempts {
				t.log.Warn("telegram send dropped after exhausting retries",
					"class", msg.class, "units", msg.units, "attempts", msg.attempts)
				t.drop()
				return
			}
			t.log.Debug("telegram send retry",
				"class", msg.class, "attempt", msg.attempts, "error", err.Error())
			t.wait(t.rectx, sendBackoff(msg.attempts))
		case outcomeFlood:
			// retry_after is Telegram's own backpressure and does not
			// consume the send-retry budget; the wait itself bounds the
			// retry rate.
			t.log.Debug("telegram flood wait", "class", msg.class,
				"retry_after_s", int(retryAfter.Seconds()))
			t.wait(t.rectx, retryAfter)
		case outcomeAmbiguous:
			// The request may have reached Telegram; a resend risks the
			// duplicate-message hazard, so the message drops.
			t.log.Warn("telegram send dropped: ambiguous delivery, no resend",
				"class", msg.class, "units", msg.units, "error", err.Error())
			t.drop()
			return
		case outcomePermanent:
			t.log.Warn("telegram send dropped: permanent failure",
				"class", msg.class, "units", msg.units, "error", err.Error())
			t.drop()
			return
		}
	}
}

// classifySendError sorts a send failure into the retry-class taxonomy.
// The library wraps sentinels and network errors with %w, so errors.Is and
// errors.As unwrap the production error chain.
func classifySendError(err error) (sendOutcome, time.Duration) {
	var flood *bot.TooManyRequestsError
	switch {
	case errors.As(err, &flood):
		return outcomeFlood, time.Duration(flood.RetryAfter) * time.Second
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The Close-drain cancellation and the library's own timeout
		// cancellation both read as ambiguity: the outcome is unknown.
		return outcomeAmbiguous, 0
	case errors.Is(err, syscall.ECONNRESET):
		// The connection reset before a response: proven non-delivery.
		return outcomeRetry, 0
	case errors.Is(err, bot.ErrorBadRequest),
		errors.Is(err, bot.ErrorForbidden),
		errors.Is(err, bot.ErrorNotFound),
		errors.Is(err, bot.ErrorUnauthorized),
		errors.Is(err, bot.ErrorConflict):
		return outcomePermanent, 0
	}
	if _, ok := errors.AsType[*bot.MigrateError](err); ok {
		return outcomePermanent, 0
	}
	if _, ok := errors.AsType[*telegramServerError](err); ok {
		return outcomeRetry, 0
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return outcomeAmbiguous, 0
	}
	// Default to ambiguity: the request may have reached Telegram, so a
	// resend risks the duplicate-message hazard.
	return outcomeAmbiguous, 0
}

// sendOutcome is the retry classification of one send failure.
type sendOutcome int

const (
	outcomeRetry     sendOutcome = iota // proven transient: back off within the budget
	outcomeFlood                        // 429 with retry_after: wait, budget untouched
	outcomeAmbiguous                    // delivery unknown: drop, never resend
	outcomePermanent                    // 400/403/404-class: drop, no retry
)

// sendBackoff is the transient-retry backoff: 1s, 2s, 4s, 8s, capped.
func sendBackoff(attempt int) time.Duration {
	backoff := time.Second << (attempt - 1)
	if backoff > sendBackoffCap || backoff <= 0 {
		return sendBackoffCap
	}
	return backoff
}

// defaultWait is the interruptible retry sleep.
func defaultWait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// telegramServerError reports an HTTP 5xx response, which the design treats
// as proven non-delivery. The library's default branch wraps such responses
// in an unclassified error string, so the injected RoundTripper replaces
// them with a typed error the classifier can unwrap.
// deliberate: the typed error carries the status code only, discarding the
// response body; a binary/log redesign of the library's error mapping is
// the upgrade path.
type telegramServerError struct {
	code int
}

func (e *telegramServerError) Error() string {
	return fmt.Sprintf("telegram server error %d", e.code)
}

// serverStatusTransport converts 5xx responses to telegramServerError before
// the library decodes them.
type serverStatusTransport struct {
	base http.RoundTripper
}

func (t *serverStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil && resp.StatusCode >= 500 {
		code := resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &telegramServerError{code: code}
	}
	return resp, err
}
