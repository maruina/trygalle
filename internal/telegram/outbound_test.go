package telegram

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// newBotTransport wires a transport to the real library client against a
// mock Bot API server, exactly as production New wires it: a
// serverStatusTransport under the HTTP client so 5xx responses classify as
// telegramServerError before the library decodes them. The returned log
// records every send attempt the mock server observes, so a test can tell
// attempts from deliveries.
func newBotTransport(t testing.TB, log *slog.Logger, wait func(ctx context.Context, d time.Duration), handler func(w http.ResponseWriter, srv *botServerLog)) (*Transport, *botServerLog) {
	t.Helper()
	srv := &botServerLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(t, r)
		handler(w, srv)
	}))
	t.Cleanup(server.Close)

	cfg, err := (&Config{Token: "1:test-token", AllowedUser: 42}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   cfg.PollTimeout + clientTimeoutMargin,
		Transport: &serverStatusTransport{base: http.DefaultTransport},
	}
	b, err := bot.New(cfg.Token,
		bot.WithServerURL(server.URL),
		bot.WithSkipGetMe(),
		bot.WithHTTPClient(cfg.PollTimeout, client),
	)
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return newTransport(cfg, b, log, wait), srv
}

// botServerLog is the mock Bot API server's sendMessage log: the parsed form
// fields of every observed request plus the success-answer count.
type botServerLog struct {
	mu   sync.Mutex
	reqs []recordedRequest
	// successes counts the attempts the server answered with a success
	// body; the server-side proxy for delivered exactly once.
	successes int
}

type recordedRequest struct {
	path   string
	fields map[string][]string
}

func (s *botServerLog) record(t testing.TB, r *http.Request) {
	t.Helper()
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Errorf("mock server: parse form: %v", err)
	}
	fields := map[string][]string{}
	if r.MultipartForm != nil {
		for name, vals := range r.MultipartForm.Value {
			fields[name] = slices.Clone(vals)
		}
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, recordedRequest{path: r.URL.Path, fields: fields})
	s.mu.Unlock()
}

func (s *botServerLog) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *botServerLog) deliveries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.successes
}

// lastText returns the text field of the last recorded request.
func (s *botServerLog) lastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1].fields["text"][0]
}

// okSend answers one sendMessage with a minimal success body.
func (s *botServerLog) okSend(w http.ResponseWriter, text string) {
	s.mu.Lock()
	s.successes++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"},"text":"` + text + `"}}`))
}

// errSend answers one sendMessage with a Bot API error body paired with its
// HTTP status, the shape both classification paths read.
func errSend(w http.ResponseWriter, code int, description string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":false,"error_code":` + strconv.Itoa(code) + `,"description":"` + description + `"}`))
}

// floodSend answers one sendMessage with the 429 shape the library parses
// retry_after from.
func floodSend(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":` + strconv.Itoa(retryAfter) + `}}`))
}

func TestLibraryWireFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tt, srv := newBotTransport(t, nil, nil, func(w http.ResponseWriter, srv *botServerLog) {
		srv.okSend(w, "plain `text`")
	})
	tt.Response(ctx, "plain `text`")

	waitFor(t, 5*time.Second, func() bool { return len(srv.requests()) == 1 }, "the sendMessage request to reach the mock server")
	reqs := srv.requests()
	if want := "/bot1:test-token/sendMessage"; reqs[0].path != want {
		t.Errorf("request path %q, want %q", reqs[0].path, want)
	}
	if got := reqs[0].fields["chat_id"]; len(got) != 1 || got[0] != "42" {
		t.Errorf("chat_id field %v, want [42]", got)
	}
	if got := reqs[0].fields["text"]; len(got) != 1 || got[0] != "plain `text`" {
		t.Errorf("text field %v, want the exact sent text", got)
	}
	names := make([]string, 0, len(reqs[0].fields))
	for name := range reqs[0].fields {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"chat_id", "text"}) {
		t.Errorf("form fields %v, want exactly chat_id and text (no parse_mode)", names)
	}
}

func TestLibraryServerErrorRetriesAsProvenTransient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	waits := &waitsRecord{}

	attempts := 0
	tt, srv := newBotTransport(t, nil, waits.wait, func(w http.ResponseWriter, srv *botServerLog) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			errSend(w, 500, "internal error")
			return
		}
		srv.okSend(w, "flaky answer")
	})
	tt.Response(ctx, "flaky answer")

	waitFor(t, 5*time.Second, func() bool { return srv.deliveries() == 1 }, "the delivery after the 5xx retries")
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Errorf("send attempts = %d, want two 5xx retries plus the delivery", len(reqs))
	}
	got := waits.snapshot()
	if len(got) != 2 {
		t.Errorf("recorded %d waits, want one backoff per retry", len(got))
	}
	wantWaits := []time.Duration{time.Second, 2 * time.Second}
	if !slices.Equal(got, wantWaits) {
		t.Errorf("waits %v, want the exponential backoff %v", got, wantWaits)
	}
}

func TestLibraryTooManyRequestsWaitsWithoutBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	waits := &waitsRecord{}

	attempts := 0
	tt, srv := newBotTransport(t, nil, waits.wait, func(w http.ResponseWriter, srv *botServerLog) {
		attempts++
		if attempts <= 6 {
			floodSend(w, 3)
			return
		}
		srv.okSend(w, "flood answer")
	})
	tt.Response(ctx, "flood answer")

	// Six flood responses exceed maxSendAttempts, so success proves the
	// send-retry budget was untouched.
	waitFor(t, 5*time.Second, func() bool { return srv.deliveries() == 1 }, "the delivery after the flood waits")
	reqs := srv.requests()
	if len(reqs) != 7 {
		t.Errorf("send attempts = %d, want six flood waits plus the delivery", len(reqs))
	}
	got := waits.snapshot()
	if len(got) != 6 {
		t.Errorf("recorded %d waits, want one per flood response", len(got))
	}
	for i, d := range got {
		if d != 3*time.Second {
			t.Errorf("wait %d = %s, want the retry_after duration", i, d)
		}
	}
}

func TestLibraryClientErrorsDropPermanently(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{400, 403, 404} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			capture, log := newCapture()
			tt, srv := newBotTransport(t, log, nil, func(w http.ResponseWriter, _ *botServerLog) {
				errSend(w, code, "chat not found")
			})
			tt.Response(ctx, "victim text")

			waitFor(t, 5*time.Second, func() bool { return capture.countMsg("permanent") == 1 }, "the permanent-drop log record")
			if got := srv.requests(); len(got) != 1 {
				t.Errorf("send attempts = %d, want one with no retry", len(got))
			}
			if got := srv.deliveries(); got != 0 {
				t.Errorf("deliveries = %d, want the permanent failure to drop the message", got)
			}
			if capture.contains("victim text") {
				t.Error("drop log leaked message content")
			}
		})
	}
}
