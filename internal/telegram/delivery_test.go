package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	botmodels "github.com/go-telegram/bot/models"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// resetSendError shapes the library's connection-reset error chain; the
// transport classifies it as proven transient.
func resetSendError() error {
	op := &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.ECONNRESET)}
	return &url.Error{Op: "Post", URL: "https://api.telegram.org/bot/redacted/sendMessage", Err: op}
}

// floodSendError is the 429-with-retry_after shape the library returns.
func floodSendError(retryAfter int) error {
	return &bot.TooManyRequestsError{Message: "too many requests", RetryAfter: retryAfter}
}

func permSendError() error {
	return fmt.Errorf("%w, %s", bot.ErrorForbidden, "chat forbidden")
}

// stubSender drives the sender seam so tests control send outcomes without
// library behavior. Each test fills fn with its own scripted behavior; call
// indexes sends in recording order.
type stubSender struct {
	mu      sync.Mutex
	calls   int
	sent    []string // texts recorded at successful send return
	chatIDs []int64
	fn      func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error)
}

// SendMessage advances the call index and records a successful send, so
// requirement "delivered exactly once" reads off one list.
func (s *stubSender) SendMessage(ctx context.Context, p *bot.SendMessageParams) (*botmodels.Message, error) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	s.mu.Unlock()
	msg, err := s.fn(ctx, call, p)
	if err == nil {
		s.record(p)
	}
	return msg, err
}

func (s *stubSender) record(p *bot.SendMessageParams) {
	s.mu.Lock()
	s.sent = append(s.sent, p.Text)
	if id, ok := p.ChatID.(int64); ok {
		s.chatIDs = append(s.chatIDs, id)
	}
	s.mu.Unlock()
}

func (s *stubSender) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

func (s *stubSender) callsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// chatID returns the recorded chat of the first successful send.
func (s *stubSender) chatID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chatIDs) == 0 {
		return 0
	}
	return s.chatIDs[0]
}

func (s *stubSender) hasSent(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.sent, text)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t testing.TB, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// captureHandler collects log records so tests assert metadata-only output.
type captureHandler struct {
	mu   sync.Mutex
	recs []captureRecord
}

type captureRecord struct {
	msg   string
	attrs map[string]string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := captureRecord{msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.recs = append(h.recs, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// countMsg counts records whose message contains substr.
func (h *captureHandler) countMsg(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, rec := range h.recs {
		if strings.Contains(rec.msg, substr) {
			n++
		}
	}
	return n
}

// contains reports whether any message or attribute value contains substr.
func (h *captureHandler) contains(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		if strings.Contains(rec.msg, substr) {
			return true
		}
		for _, v := range rec.attrs {
			if strings.Contains(v, substr) {
				return true
			}
		}
	}
	return false
}

func newCapture() (*captureHandler, *slog.Logger) {
	h := &captureHandler{}
	return h, slog.New(h)
}

// newStubTransport builds a transport around a stub sender. Tests that assert
// wait durations overwrite nothing after construction: wait arrives as an
// argument instead, so the delivery goroutine never races on the field. A nil
// wait is instant.
func newStubTransport(tb testing.TB, log *slog.Logger, wait func(context.Context, time.Duration), fn func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error)) (*Transport, *stubSender) {
	tb.Helper()
	cfg, err := (&Config{Token: "1:test-token", AllowedUser: 42, PollTimeout: time.Second}).Validate()
	if err != nil {
		panic(err)
	}
	if wait == nil {
		wait = func(context.Context, time.Duration) {}
	}
	s := &stubSender{fn: fn}
	tk := newTransport(cfg, s, log, wait)
	tb.Cleanup(func() { tk.Close(context.Background()) })
	return tk, s
}

// enterRelease holds one send: it signals entered, waits on release (or the
// send context's cancellation like the real HTTP client), and returns result.
type enterRelease struct {
	entered chan struct{}
	release chan struct{}
	result  error
}

func (e enterRelease) apply(ctx context.Context) error {
	if e.entered != nil {
		select {
		case e.entered <- struct{}{}:
		default:
		}
	}
	select {
	case <-e.release:
		return e.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

func okSend() (*botmodels.Message, error) { return &botmodels.Message{}, nil }

func statusNotice(pending int) runtime.Notice {
	state := &rpc.State{
		SessionID:           "s-123",
		IsStreaming:         true,
		PendingMessageCount: pending,
		Model:               &rpc.Model{ID: "gpt-5"},
	}
	return runtime.Notice{Kind: runtime.NoticeStatus, State: state}
}

func statusWording(pending int) string {
	return fmt.Sprintf("Working. Queue %d. Model gpt-5.", pending)
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		give       Config
		wantErrTok string
		wantPoll   time.Duration
	}{
		{
			name:       "missing token",
			give:       Config{AllowedUser: 42, PollTimeout: time.Second},
			wantErrTok: "TRYGALLE_TELEGRAM_TOKEN",
		},
		{
			name:       "missing allowed user",
			give:       Config{Token: "1:test", PollTimeout: time.Second},
			wantErrTok: "TRYGALLE_TELEGRAM_ALLOWED_USER",
		},
		{
			name:     "poll timeout defaults",
			give:     Config{Token: "1:test", AllowedUser: 42},
			wantPoll: defaultPollTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.give.Validate()
			if tt.wantErrTok == "" {
				if err != nil {
					t.Fatalf("want success, got error %v", err)
				}
				if got.PollTimeout != tt.wantPoll {
					t.Errorf("PollTimeout = %v, want %v", got.PollTimeout, tt.wantPoll)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %s", tt.wantErrTok)
			}
			if !strings.Contains(err.Error(), tt.wantErrTok) {
				t.Errorf("error %v does not mention %s", err, tt.wantErrTok)
			}
			if tt.give.Token != "" && strings.Contains(err.Error(), tt.give.Token) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestNoticeNonBlocking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	capture, log := newCapture()

	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call == 0 {
			if err := (enterRelease{entered: entered, release: release}).apply(ctx); err != nil {
				return nil, err
			}
		}
		return okSend()
	})

	start := time.Now()
	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeExtensionHandled})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Notice took %s to return under a blocked sender", elapsed)
	}
	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, "the send to be entered while Notice already returned")
	close(release)
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 1 }, "delivery after the sender unblocks")
	if got := s.chatID(); got != 42 {
		t.Errorf("chat id %d, want the allowlisted user", got)
	}
	if got := s.delivered(); got[0] != "Handled by the extension; no run started." {
		t.Errorf("delivered %q, want the rendered ExtensionHandled wording", got[0])
	}
	if capture.contains("1:test-token") {
		t.Error("logs leaked the token")
	}
}

func TestSupersedeHeldStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	tt, s := newStubTransport(t, nil, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call == 0 {
			return nil, (enterRelease{entered: entered, release: release, result: resetSendError()}).apply(ctx)
		}
		return okSend()
	})

	tt.Notice(ctx, statusNotice(2))
	waitFor(t, 2*time.Second, func() bool { return s.callsMade() == 1 }, "the older status send to be entered")
	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeSteerAcknowledged})
	tt.Response(ctx, "answer text")
	tt.Notice(ctx, statusNotice(5))

	close(release)
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 3 }, "FIFO delivery after the network recovers")
	got := s.delivered()
	want := "Queued into the current run.|answer text|" + statusWording(5)
	if strings.Join(got, "|") != want {
		t.Errorf("delivered order %q, want %q", got, want)
	}
	if s.hasSent(statusWording(2)) {
		t.Errorf("the superseded older status was sent: %v", got)
	}
}

func TestTransientFailureHoldsThenDelivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capture, log := newCapture()
	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call < 3 {
			return nil, resetSendError()
		}
		return okSend()
	})
	tt.Response(ctx, "held answer")
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 1 }, "delivery after transient failures")
	if count := s.callsMade(); count != 4 {
		t.Errorf("send calls = %d, want three retries plus the delivery", count)
	}
	if got := s.delivered(); strings.Join(got, "") != "held answer" {
		t.Errorf("delivered %q exactly %d times, want the single held answer", got, len(got))
	}
	if n := capture.countMsg("send retry"); n != 3 {
		t.Errorf("retry log records = %d, want one metadata record per retried attempt", n)
	}
	if capture.contains("held answer") {
		t.Error("retry logs leaked message content")
	}
}

func TestFloodRetryAfterDoesNotBurnBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	waits := &waitsRecord{}

	tt, s := newStubTransport(t, nil, waits.wait, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call < 6 {
			return nil, floodSendError(1)
		}
		return okSend()
	})

	tt.Response(ctx, "flood answer")
	waitFor(t, 3*time.Second, func() bool { return len(s.delivered()) == 1 }, "delivery after flood retries")
	if count := s.callsMade(); count != 7 {
		t.Errorf("send calls = %d, want six flood waits plus the delivery, proving the budget was not burned", count)
	}
	got := waits.snapshot()
	if len(got) != 6 {
		t.Errorf("recorded %d waits, want one per flood response", len(got))
	}
	for i, d := range got {
		if d != time.Second {
			t.Errorf("wait %d = %s, want the retry_after duration", i, d)
		}
	}
}

// waitsRecord records wait durations with synchronization the transport can
// share safely from construction time.
type waitsRecord struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *waitsRecord) wait(_ context.Context, d time.Duration) {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
}

func (w *waitsRecord) snapshot() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

func TestAmbiguousTimeoutDropsWithoutResend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capture, log := newCapture()
	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		return nil, context.DeadlineExceeded
	})
	tt.Response(ctx, "timeout victim")
	waitFor(t, 2*time.Second, func() bool { return capture.countMsg("ambiguous") == 1 }, "the ambiguous-drop log record")
	if count := s.callsMade(); count != 1 {
		t.Errorf("send calls = %d, want exactly one attempt with no resend", count)
	}
	if got := s.delivered(); len(got) != 0 {
		t.Errorf("delivered %q, want nothing", got)
	}
}

func TestPermanentFailureDropsWithoutRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capture, log := newCapture()
	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		return nil, permSendError()
	})
	tt.Response(ctx, "forbidden victim")
	waitFor(t, 2*time.Second, func() bool { return capture.countMsg("permanent") == 1 }, "the permanent-drop log record")
	if count := s.callsMade(); count != 1 {
		t.Errorf("send calls = %d, want one attempt with no retry", count)
	}
	if got := s.delivered(); len(got) != 0 {
		t.Errorf("delivered %q, want nothing", got)
	}
}

func TestRetryBudgetExhaustionDropsWithMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capture, log := newCapture()
	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		return nil, resetSendError()
	})
	tt.Response(ctx, "RETRY_MARKER")
	waitFor(t, 2*time.Second, func() bool { return capture.countMsg("exhaust") == 1 }, "the exhaustion log record")
	if count := s.callsMade(); count != maxSendAttempts {
		t.Errorf("send calls = %d, want the %d-attempt budget to bound the work", count, maxSendAttempts)
	}
	if got := s.delivered(); len(got) != 0 {
		t.Errorf("delivered %q, want nothing", got)
	}
	if capture.contains("RETRY_MARKER") {
		t.Error("exhaustion logs leaked message content")
	}
}

func TestHeldQueueCapEvictsOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capture, log := newCapture()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call == 0 {
			if err := (enterRelease{entered: entered, release: release}).apply(ctx); err != nil {
				return nil, err
			}
		}
		return okSend()
	})
	tt.Response(ctx, "oldest current message")
	waitFor(t, 2*time.Second, func() bool { return s.callsMade() == 1 }, "the current send to be entered")
	for i := 1; i <= 52; i++ {
		tt.Response(ctx, fmt.Sprintf("held %d", i))
	}
	close(release)
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 50 }, "delivery bounded by the held cap")
	got := s.delivered()
	if got[0] != "oldest current message" {
		t.Errorf("first delivery %q, want the in-flight head", got[0])
	}
	if got[len(got)-1] != "held 52" {
		t.Errorf("last delivery %q, want the newest held message", got[len(got)-1])
	}
	for _, evicted := range []string{"held 1", "held 2", "held 3"} {
		if s.hasSent(evicted) {
			t.Errorf("evicted message %q was delivered: %v", evicted, got)
		}
	}
	if n := capture.countMsg("evict"); n != 3 {
		t.Errorf("eviction warnings = %d, want one per evicted message", n)
	}
}

func TestCloseRestartNoticeSingleAttemptOnFailure(t *testing.T) {
	t.Parallel()
	capture, log := newCapture()

	tt, s := newStubTransport(t, log, nil, func(context.Context, int, *bot.SendMessageParams) (*botmodels.Message, error) {
		return nil, resetSendError()
	})
	tt.Notice(context.Background(), runtime.Notice{Kind: runtime.NoticeRestartNotice})

	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tt.Close(closeCtx)

	if calls := s.callsMade(); calls != 1 {
		t.Errorf("send calls = %d, want the restart notice's single attempt with no retry", calls)
	}
	if got := s.delivered(); len(got) != 0 {
		t.Errorf("delivered %q, want the failed restart notice to drop", got)
	}
	if n := capture.countMsg("once message dropped"); n != 1 {
		t.Errorf("once-drop log records = %d, want metadata-only logging of the failed attempt", n)
	}
	if capture.contains("Restarting; active operation aborted.") {
		t.Error("failure log leaked notice content")
	}
}

func TestCloseDeliversRestartNoticeAndDropsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, log := newCapture()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	tt, s := newStubTransport(t, log, nil, func(ctx context.Context, call int, p *bot.SendMessageParams) (*botmodels.Message, error) {
		if call == 0 {
			return nil, (enterRelease{entered: entered, release: release, result: resetSendError()}).apply(ctx)
		}
		if p.Text == "Restarting; active operation aborted." {
			return okSend()
		}
		return nil, resetSendError()
	})
	tt.Response(ctx, "held for the drain")
	waitFor(t, 2*time.Second, func() bool { return s.callsMade() == 1 }, "the held send to be entered")
	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeRestartNotice})

	close(release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tt.Close(closeCtx)

	got := s.delivered()
	if len(got) != 1 || got[0] != "Restarting; active operation aborted." {
		t.Errorf("delivered %q, want only the restart notice's single attempt", got)
	}
	if s.hasSent("held for the drain") {
		t.Errorf("held message was delivered past close: %v", got)
	}
}
