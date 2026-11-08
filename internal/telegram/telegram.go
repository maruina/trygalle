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
		cfg:      cfg,
		sender:   send,
		log:      log,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		rectx:    rectx,
		recancel: recancel,
		wait:     wait,
	}
	go t.run()
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
	once := n.Kind == runtime.NoticeRestartNotice
	supersede := n.Kind == runtime.NoticeStatus || n.Kind == runtime.NoticeHeartbeat
	t.enqueueBatch(string(n.Kind), renderNotice(n), once, supersede)
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
