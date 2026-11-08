package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/maruina/trygalle/internal/runtime"
)

// handler is the inbound seam: the poller forwards every text message
// verbatim — including reserved-command text such as /status — and the
// coordinator owns classification. HandleUserInput returns after its RPC
// response, never after operation settlement, so the loop keeps polling
// during a run.
type handler interface {
	HandleUserInput(ctx context.Context, text string) error
}

var _ handler = (*runtime.Coordinator)(nil)

// defaultAPIURL is the production Bot API base.
// deliberate: pinned to the public Bot API; a self-hosted Bot API gateway
// joins the configuration surface only when a deployment asks for it.
const defaultAPIURL = "https://api.telegram.org"

// pollBackoff is the brief pause after a recoverable poll failure (a
// network error, 5xx, or malformed response); it cannot pin the loop
// because every wait honours the cancel context.
// deliberate: a fixed pause; tuning joins the retry constants on beta
// evidence (follow-up #5).
const pollBackoff = time.Second

// nonTextReply is the single canned fixed string for media and other
// non-text updates, kept outside the notice taxonomy (design decision 3:
// one string, not a media-kind split).
// deliberate: a review-time styling choice, unpinned by the wording table;
// a wording change is an edit to this constant and its test.
const nonTextReply = "I can only process text messages for now."

// poller owns the transport-owned long-poll loop. The pinned library's only
// polling surface advances offsets before handlers run, so this loop is the
// load-bearing replacement: the offset lives in the caller's memory and
// moves only after an update is fully handled.
type poller struct {
	cfg Config

	// url is the Bot API base; tests point it at the mock server.
	url    string
	client *http.Client

	send   sender
	handle handler
	log    *slog.Logger

	// wait sleeps between poll attempts (backoff or retry_after); injected
	// so tests run instantly.
	wait func(ctx context.Context, d time.Duration)
}

// newPoller builds the loop around the stdlib getUpdates client. The HTTP
// timeout covers the long poll plus a margin, so a stalled connection
// cannot pin the loop. wait nil selects the default interruptible sleep.
func newPoller(cfg Config, send sender, handle handler, wait func(ctx context.Context, d time.Duration), log *slog.Logger) *poller {
	if log == nil {
		log = slog.Default()
	}
	if wait == nil {
		wait = defaultWait
	}
	return &poller{
		cfg: cfg,
		url: defaultAPIURL,
		client: &http.Client{
			Timeout:   cfg.PollTimeout + clientTimeoutMargin,
			Transport: &serverStatusTransport{base: http.DefaultTransport},
		},
		send:   send,
		handle: handle,
		log:    log,
		wait:   wait,
	}
}

// pollConflictError reports a getUpdates failure no retry can fix: another
// poller holds the token (409, the getUpdates conflict) or the token is
// unauthorized (401). Both are configuration errors: the composition logs
// and exits non-zero, and recovery is operator action.
type pollConflictError struct {
	code        int
	description string
}

func (e *pollConflictError) Error() string {
	return fmt.Sprintf("telegram: unrecoverable getUpdates HTTP %d: %s", e.code, e.description)
}

// pollFloodError reports HTTP 429 with Telegram's retry_after backpressure
// parameter.
type pollFloodError struct {
	d           time.Duration
	description string
}

func (e *pollFloodError) Error() string {
	return fmt.Sprintf("telegram: getUpdates backpressure, retry after %s: %s", e.d, e.description)
}

// Run polls getUpdates until ctx is cancelled (nil return) or a fatal
// failure surfaces. The loop starts at offset 0 without
// drop_pending_updates: a restart replays the un-acked backlog
// at-least-once. A non-nil handler return stops the loop before the offset
// advances, so the un-acked update re-runs after a process restart.
//
// Failure classes: 409/401 is a configuration conflict and exits non-zero;
// 429 waits the response's retry_after, then repolls (not a failure, so
// it never advances the stall counter); anything else backs off briefly,
// and the fifth consecutive recoverable failure logs the one polling-stall
// warning.
// RunPoll drives the transport-owned long-poll loop against the
// coordinator: the production inbound seam over the unexported poller. It
// blocks until ctx is cancelled (nil, the shutdown path) or a fatal failure
// surfaces — a 409/401 poll conflict, or a non-nil handler return, which
// the design reads as coordinator death. Returns after the handler error's
// own processing, not after operation settlement.
func (t *Transport) RunPoll(ctx context.Context, coord *runtime.Coordinator) error {
	return newPoller(t.cfg, t.sender, coord, nil, t.log).Run(ctx)
}

func (p *poller) Run(ctx context.Context) error {
	p.log.InfoContext(ctx, "poll loop started")
	defer p.log.InfoContext(ctx, "poll loop stopped")
	offset := int64(0)
	failures := 0
	for {
		updates, err := p.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if conflict, ok := errors.AsType[*pollConflictError](err); ok {
				p.log.WarnContext(ctx, "poll configuration conflict", "code", conflict.code)
				return err
			}
			if flood, ok := errors.AsType[*pollFloodError](err); ok {
				p.log.InfoContext(ctx, "poll backpressure", "retry_after", flood.d.Round(time.Second).String())
				p.wait(ctx, flood.d)
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			failures++
			if failures == pollStallFailures {
				p.log.WarnContext(ctx, "polling stall", "consecutive_failures", failures)
			}
			p.wait(ctx, pollBackoff)
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		failures = 0
		if offset == 0 && len(updates) > 0 {
			p.log.InfoContext(ctx, "backlog replay", "updates", len(updates))
		}
		for _, u := range updates {
			next, err := p.handleUpdate(ctx, u)
			if err != nil {
				// deliberate: error handling returns without assigning
				// next, so the offset stays on the un-acked update.
				return err
			}
			offset = next
		}
	}
}

// handleUpdate classifies one update and returns the offset past it. The
// allowlist acts before any processing, including before the canned reply.
// Text messages forward with no echo back through this path: Pi's response
// and notices reach the chat through the transport's Notifier methods.
func (p *poller) handleUpdate(ctx context.Context, u models.Update) (int64, error) {
	next := u.ID + 1
	if m := u.Message; m != nil {
		// ChatShape check order: identity first, then chat type, so a
		// stranger's content in any chat is dropped unprocessed.
		from := int64(0)
		if m.From != nil {
			from = m.From.ID
		}
		if from != p.cfg.AllowedUser || m.Chat.Type != models.ChatTypePrivate {
			p.log.InfoContext(ctx, "allowlist rejection", "chat_type", string(m.Chat.Type))
			return next, nil
		}
		if m.Text == "" {
			p.reply(ctx, nonTextReply)
			return next, nil
		}
		if err := p.handle.HandleUserInput(ctx, m.Text); err != nil {
			return 0, err
		}
		return next, nil
	}
	// deliberate: non-message update kinds carry no per-kind identity check
	// here; sender identification for those shapes joins the future media
	// seam design, so they get the same canned reply as media messages.
	p.reply(ctx, nonTextReply)
	return next, nil
}

// reply sends one fixed reply through the sender seam with a single
// attempt; a failure is a metadata-only log, never a retry.
// deliberate: no send-retry budget for canned replies — a lost reply is
// re-offered only by the at-least-once replay after a restart.
func (p *poller) reply(ctx context.Context, text string) {
	params := &bot.SendMessageParams{ChatID: p.cfg.AllowedUser, Text: text}
	if _, err := p.send.SendMessage(ctx, params); err != nil {
		p.log.WarnContext(ctx, "canned reply send failed", "error", err)
	}
}

// getUpdates performs one long-poll request on the stdlib JSON client.
// allowed_updates and drop_pending_updates stay unset per the design; the
// response decodes into the library's models.Update so both pipelines share
// one update shape.
func (p *poller) getUpdates(ctx context.Context, offset int64) ([]models.Update, error) {
	form := url.Values{}
	form.Set("offset", strconv.FormatInt(offset, 10))
	form.Set("timeout", strconv.Itoa(int(p.cfg.PollTimeout/time.Second)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.url+"/bot"+p.cfg.Token+"/getUpdates", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env struct {
		Ok          bool            `json:"ok"`
		Result      []models.Update `json:"result"`
		ErrorCode   int             `json:"error_code,omitempty"`
		Description string          `json:"description,omitempty"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if !env.Ok {
		return nil, classifyPollError(env.ErrorCode, env.Parameters, env.Description, resp.StatusCode)
	}
	return env.Result, nil
}

// classifyPollError sorts one ok:false response into the design's failure
// taxonomy: 409 and 401 are fatal configuration errors, 429 carries the
// retry_after backpressure protocol, and every other code is a recoverable
// failure the loop backs off from.
func classifyPollError(code int, params *struct {
	RetryAfter int `json:"retry_after"`
}, description string, httpStatus int) error {
	switch code {
	case http.StatusConflict, http.StatusUnauthorized:
		// deliberate: classify on the body's error_code, not the HTTP
		// status, because the serverStatusTransport in the client path has
		// already turned non-4xx statuses into telegramServerError at this
		// point. Both shapes wrap Telegram's own codes.
		return &pollConflictError{code: code, description: description}
	case http.StatusTooManyRequests:
		if params == nil || params.RetryAfter <= 0 {
			break
		}
		return &pollFloodError{
			d:           time.Duration(params.RetryAfter) * time.Second,
			description: description,
		}
	}
	return fmt.Errorf("HTTP %d error %d: %s", httpStatus, code, description)
}
