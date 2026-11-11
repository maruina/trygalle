// Package telegram implements a runtime.Notifier for the Telegram Bot API.
//
// The transport owns Telegram adaptation only: outbound rendering, chunking,
// and delivery with bounded retry and hold, plus inbound long polling with
// offset discipline and a numeric allowlist. Reserved-command classification,
// operation state, and completion detection stay in the runtime coordinator.
//
// The transport depends on internal/runtime and go-telegram/bot, not on
// internal/rpc: notice payloads carry lifecycle data only.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/maruina/trygalle/internal/runtime"
)

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

// sender is the wire seam the delivery pipeline sends through. The pinned
// library owns the wire surface and future media support; the seam keeps the
// send loop testable against a stub.
type sender interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	SendChatAction(ctx context.Context, params *bot.SendChatActionParams) (bool, error)
}

var (
	_ sender           = (*bot.Bot)(nil)
	_ runtime.Notifier = (*Transport)(nil)
)

// Transport delivers Pi responses and runtime notices to one private chat.
// It implements runtime.Notifier with enqueue-only seams so the coordinator's
// event loop never waits on delivery.
type Transport struct {
	cfg    Config
	sender sender
	log    *slog.Logger

	// typing carries coalesced run-activity pulses for the typing
	// indicator goroutine; capacity one because excess pulses drop.
	typing chan struct{}

	// typing timing per transport, copied from the defaults at construction;
	// tests shrink the cadence on their own instance.
	typingTimeout       time.Duration
	typingRefresh       time.Duration
	typingRefreshWindow time.Duration
	typingSuppressAfter int

	mu        sync.Mutex
	queue     []*qmsg
	current   *qmsg // the in-flight head, still supersedeable when undelivered
	nextGroup uint64
	closed    bool // enqueue is a safe drop once set
	stopped   bool // drain mode: once-messages only, held messages discarded

	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	// rectx owns send attempts and retry waits, including the drain's
	// restart-notice attempt. It is cancelled only by Close, never by a
	// cancelled signal context: the drain must outlive signal delivery.
	rectx    context.Context
	recancel context.CancelFunc

	// wait sleeps between attempts (transient backoff or flood retry_after);
	// injected so tests retry instantly.
	wait func(ctx context.Context, d time.Duration)
}

// clientTimeoutMargin is added to the poll timeout for the bot client's HTTP
// timeout, so a hung connection cannot pin delivery.
// deliberate: a fixed margin keeping one shared client simple; the send
// context (Close-bounded) remains the true exit bound.
const clientTimeoutMargin = 30 * time.Second

// New builds the production transport around the pinned library client with
// WithSkipGetMe so construction never touches the network.
func New(cfg Config, log *slog.Logger) (*Transport, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:   cfg.PollTimeout + clientTimeoutMargin,
		Transport: &serverStatusTransport{base: http.DefaultTransport},
	}
	b, err := bot.New(cfg.Token,
		bot.WithSkipGetMe(),
		bot.WithHTTPClient(cfg.PollTimeout, client),
	)
	if err != nil {
		return nil, fmt.Errorf("telegram: create bot client: %w", err)
	}
	return newTransport(cfg, b, log, nil), nil
}

// newTransport wires the delivery state and starts the single delivery
// goroutine. wait nil selects the default interruptible sleep; tests inject
// bounded waits.
func newTransport(cfg Config, send sender, log *slog.Logger, wait func(ctx context.Context, d time.Duration)) *Transport {
	if log == nil {
		log = slog.Default()
	}
	if wait == nil {
		wait = defaultWait
	}
	rectx, recancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:    cfg,
		sender: send,
		log:    log,
		wake:   make(chan struct{}, 1),

		typingTimeout:       defaultTypingTimeout,
		typingRefresh:       defaultTypingRefresh,
		typingRefreshWindow: defaultTypingRefreshWindow,
		typingSuppressAfter: defaultTypingSuppressAfter,
		stop:                make(chan struct{}),
		done:                make(chan struct{}),
		typing:              make(chan struct{}, 1),
		rectx:               rectx,
		recancel:            recancel,
		wait:                wait,
	}
	go t.run()
	go t.runTyping()
	return t
}

// Response enqueues Pi's final response as received. Chunks render, split,
// and join the hold queue FIFO.
func (t *Transport) Response(_ context.Context, text string) {
	t.enqueueBatch(classResponse, renderResponse(text), false, false)
}

// Notice enqueues one notice kind per the pinned wording table. Recurring
// kinds supersede pending same-kind messages; the restart notice bypasses
// holds for its single drain attempt.
func (t *Transport) Notice(_ context.Context, n runtime.Notice) {
	if n.Kind == runtime.NoticeWorking {
		// The run-activity signal is a transient typing indicator, never a
		// chat message: pulses coalesce and fire-and-forget so the
		// coordinator event loop never waits on the wire.
		t.pulseTyping()
		return
	}
	once := n.Kind == runtime.NoticeRestartNotice
	supersede := n.Kind == runtime.NoticeStatus || n.Kind == runtime.NoticeHeartbeat
	t.enqueueBatch(string(n.Kind), renderNotice(n), once, supersede)
}

// pulseTyping coalesces at most one pending indicator; excess pulses drop
// because the next activity milestone re-pulses anyway.
func (t *Transport) pulseTyping() {
	select {
	case t.typing <- struct{}{}:
	default:
	}
}

// Typing indicator timing defaults, shaped on ZeroClaw's channel scope and
// OpenClaw's failure hardening (evidence in the telegram plan): Telegram
// expires the indicator after 5s, so an open scope re-sends every 4s; a
// scope with no activity for the window idles back to the pulse channel;
// after the failure threshold the scope stops sending, because OpenClaw's
// changelog pins repeated 401 sendChatAction loops as a Telegram
// abuse-enforcement risk. deliberate: defaults are package vars copied into
// per-transport fields, so tests shrink the cadence without racing parallel
// tests.
var (
	defaultTypingTimeout       = 10 * time.Second
	defaultTypingRefresh       = 4 * time.Second
	defaultTypingRefreshWindow = 12 * time.Second
	defaultTypingSuppressAfter = 3
)

// runTyping drains the coalesced indicator pulses. Each pulse opens one
// activity scope; Close ends everything. A send racing the cancel fails with
// a metadata-only log.
func (t *Transport) runTyping() {
	for {
		select {
		case <-t.stop:
			return
		case <-t.typing:
			if t.runTypingScope() {
				return
			}
		}
	}
}

// runTypingScope holds the indicator alive for one activity scope: one send
// on entry, re-sends every typingRefresh while the scope is live, and an
// idle exit once the window lapses with no fresh pulse. It reports whether
// the transport is closing.
func (t *Transport) runTypingScope() (closing bool) {
	failures := 0
	send := func() {
		if failures >= t.typingSuppressAfter {
			return
		}
		ctx, cancel := context.WithTimeout(t.rectx, t.typingTimeout)
		_, err := t.sender.SendChatAction(ctx, &bot.SendChatActionParams{
			ChatID: t.cfg.AllowedUser,
			Action: models.ChatActionTyping,
		})
		cancel()
		if err != nil {
			failures++
			if failures == t.typingSuppressAfter {
				t.log.Warn("typing indicator suppressed after consecutive failures", "error", err)
			} else {
				t.log.Warn("typing indicator send failed", "error", err)
			}
			return
		}
		failures = 0
	}
	send()
	refresh := time.NewTimer(t.typingRefresh)
	defer refresh.Stop()
	window := time.NewTimer(t.typingRefreshWindow)
	defer window.Stop()
	for {
		select {
		case <-t.stop:
			return true
		case <-t.typing:
			// A fresh pulse extends the window and restarts the cadence.
			failures = 0
			resetTimer(window, t.typingRefreshWindow)
			resetTimer(refresh, t.typingRefresh)
			send()
		case <-refresh.C:
			send()
			resetTimer(refresh, t.typingRefresh)
		case <-window.C:
			return false
		}
	}
}

// resetTimer stops tm, drains a fired value if one landed, and restarts the
// cadence; safe on Go 1.23+ synchronous timer channels.
func resetTimer(tm *time.Timer, d time.Duration) {
	if !tm.Stop() {
		select {
		case <-tm.C:
		default:
		}
	}
	tm.Reset(d)
}

// Close finishes delivery. Enqueue closes first (later calls are safe
// drops), the delivery goroutine discards held messages (lost with the
// process, design-accepted) and drains the in-flight send within the retry
// budget, then finishes; Close returns once it exits or ctx's window ends,
// cancelling the retry context so a blocked send cannot pin the exit. The
// composition calls Close after Coordinator.Shutdown so the coordinator's
// best-effort restart notice gets its single attempt inside the window.
func (t *Transport) Close(ctx context.Context) {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		t.stopped = true
		close(t.stop)
	}
	t.mu.Unlock()
	defer t.recancel()
	select {
	case <-t.done:
	case <-ctx.Done():
	}
}

// classResponse is the log class of Pi's final response.
const classResponse = "response"
