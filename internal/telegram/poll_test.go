package telegram

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeHandler records the texts the poll loop forwards and can fail every
// call, standing in for the coordinator's happy and fatal paths.
type fakeHandler struct {
	err error

	mu    sync.Mutex
	texts []string
}

func (f *fakeHandler) HandleUserInput(ctx context.Context, text string) error {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	return f.err
}

func (f *fakeHandler) forwarded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.texts)
}

func (f *fakeHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.texts)
}

// runPoll runs the loop and returns a channel carrying its outcome, so a
// test controls when the loop's return is observed.
func runPoll(p *poller, ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- p.Run(ctx) }()
	return errc
}

func TestPollReplaysBacklogInOrder(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := newPollServer(t, []pollEntry{{
		batch: []string{
			textUpdateJSON(1, 42, 42, "private", "hello"),
			textUpdateJSON(2, 42, 42, "private", "/status"),
			textUpdateJSON(3, 42, 42, "private", "steer this"),
		},
	}}, map[int]<-chan struct{}{1: block})

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := runPoll(p, ctx)
	waitFor(t, 5*time.Second, func() bool { return h.count() == 3 }, "the three backlog updates to reach the handler")
	waitFor(t, 5*time.Second, func() bool { return len(srv.offsetsRecorded()) == 2 }, "the second getUpdates call to arrive")

	cancel()
	close(block)
	if err := <-errc; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.forwarded(); !slices.Equal(got, []string{"hello", "/status", "steer this"}) {
		t.Errorf("forwarded %q, want updates 1-3 in order with /status verbatim", got)
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0", "4"}) {
		t.Errorf("offsets %q, want [0 4]: start from zero, and advance only after a nil handler return", got)
	}
	if n := capture.countMsg("backlog replay"); n != 1 {
		t.Errorf("backlog replay records = %d, want 1", n)
	}
	if capture.contains("hello") {
		t.Error("message content reached the log")
	}
	if got := srv.sendCount(); got != 0 {
		t.Errorf("text updates triggered %d replies, want 0", got)
	}
}

func TestPollDropsAllowlistViolationsWithoutContent(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	// Order pins "allowlist enforcement before any processing, including
	// before the canned reply": the stranger's voice note must not reply.
	srv := newPollServer(t, []pollEntry{{
		batch: []string{
			textUpdateJSON(1, 999, 42, "private", "stranger text"),
			textUpdateJSON(2, 42, 42, "group", "group text"),
			voiceUpdateJSON(3, 999, 42, "private"),
			textUpdateJSON(4, 42, 42, "private", "allowed"),
		},
	}}, map[int]<-chan struct{}{1: block})

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := runPoll(p, ctx)
	waitFor(t, 5*time.Second, func() bool { return h.count() == 1 }, "the allowed text to reach the handler")
	waitFor(t, 5*time.Second, func() bool { return len(srv.offsetsRecorded()) == 2 }, "the second getUpdates call to arrive")

	cancel()
	close(block)
	if err := <-errc; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.forwarded(); !slices.Equal(got, []string{"allowed"}) {
		t.Errorf("forwarded %q, want only the allowed text", got)
	}
	if got := srv.sendCount(); got != 0 {
		t.Errorf("dropped updates triggered %d replies, want 0", got)
	}
	if n := capture.countMsg("allowlist rejection"); n != 3 {
		t.Errorf("allowlist rejection records = %d, want 3", n)
	}
	if capture.contains("stranger text") || capture.contains("group text") {
		t.Error("dropped message content reached the log")
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0", "5"}) {
		t.Errorf("offsets %q, want [0 5]: dropped updates still advance the offset", got)
	}
}

func TestPollAnswersNonTextUpdatesWithOneCannedReply(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	// A voice note and a callback query both classify centrally to the same
	// canned string outside the message path.
	srv := newPollServer(t, []pollEntry{{
		batch: []string{
			voiceUpdateJSON(1, 42, 42, "private"),
			callbackUpdateJSON(2, 42),
		},
	}}, map[int]<-chan struct{}{1: block})

	_, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := runPoll(p, ctx)
	waitFor(t, 5*time.Second, func() bool { return srv.sendCount() == 2 }, "the two canned replies")
	waitFor(t, 5*time.Second, func() bool { return len(srv.offsetsRecorded()) == 2 }, "the second getUpdates call to arrive")

	cancel()
	close(block)
	if err := <-errc; err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := h.count(); n != 0 {
		t.Errorf("handler received %d texts, want 0 for non-text updates", n)
	}
	_ = log
	for i := range 2 {
		if got := srv.sendText(i); got != nonTextReply {
			t.Errorf("replies[%d] = %q, want the one canned string", i, got)
		}
		if got := srv.sendChatID(i); got != "42" {
			t.Errorf("replies[%d] chat_id = %q, want the allowed user's chat", i, got)
		}
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0", "3"}) {
		t.Errorf("offsets %q, want [0 3]", got)
	}
}

func TestPollHandlerErrorStopsLoopWithoutAdvancing(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("coordinator died")
	srv := newPollServer(t, []pollEntry{{
		batch: []string{
			textUpdateJSON(5, 42, 42, "private", "one"),
			textUpdateJSON(6, 42, 42, "private", "two"),
		},
	}}, nil)

	_, log := newCapture()
	h := &fakeHandler{err: sentinel}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	errc := runPoll(p, t.Context())
	select {
	case err := <-errc:
		if !errors.Is(err, sentinel) {
			t.Fatalf("run = %v, want the handler error propagated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after a handler error")
	}
	if got := h.forwarded(); !slices.Equal(got, []string{"one"}) {
		t.Errorf("forwarded %q, want only the first update: the loop stops at the first non-nil return", got)
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0"}) {
		t.Errorf("offsets %q, want [0]: the un-acked update must not advance, so a restart replays it", got)
	}
}

func TestPollConflictExitsWithoutFurtherPoll(t *testing.T) {
	t.Parallel()
	srv := newPollServer(t, []pollEntry{{
		err: botErrorBody(409, "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"),
	}}, nil)

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	select {
	case err := <-runPoll(p, t.Context()):
		var conflict *pollConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("run = %v, want a pollConflictError", err)
		}
		if conflict.code != http.StatusConflict {
			t.Errorf("conflict code = %d, want 409", conflict.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop on a getUpdates conflict")
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0"}) {
		t.Errorf("offsets %q, want [0]: no further poll after the conflict", got)
	}
	if n := capture.countMsg("poll configuration conflict"); n != 1 {
		t.Errorf("conflict records = %d, want 1", n)
	}
}

func TestPollUnauthorizedIsFatal(t *testing.T) {
	t.Parallel()
	// The 401 is persistent: every scripted call answers unauthorized, and
	// the loop must exit anyway instead of backing off forever.
	var entries []pollEntry
	for range 5 {
		entries = append(entries, pollEntry{err: botErrorBody(http.StatusUnauthorized, "Unauthorized")})
	}
	srv := newPollServer(t, entries, nil)

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, nil, log)
	p.url = srv.server.URL

	select {
	case err := <-runPoll(p, t.Context()):
		var conflict *pollConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("run = %v, want a pollConflictError", err)
		}
		if conflict.code != http.StatusUnauthorized {
			t.Errorf("conflict code = %d, want 401", conflict.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop despite a persistent 401")
	}
	if got := srv.offsetsRecorded(); !slices.Equal(got, []string{"0"}) {
		t.Errorf("offsets %q, want [0]", got)
	}
	if n := capture.countMsg("poll configuration conflict"); n != 1 {
		t.Errorf("conflict records = %d, want 1", n)
	}
}

func TestPollBackpressureWaitsAndRepolls(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := newPollServer(t, []pollEntry{
		{err: botFloodBody(7)},
		{batch: []string{textUpdateJSON(1, 42, 42, "private", "hello")}},
	}, map[int]<-chan struct{}{2: block})

	waits := &waitsRecord{}
	_, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, waits.wait, log)
	p.url = srv.server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := runPoll(p, ctx)
	waitFor(t, 5*time.Second, func() bool { return h.count() == 1 }, "the post-backpressure update to reach the handler")
	waitFor(t, 5*time.Second, func() bool { return len(srv.offsetsRecorded()) == 3 }, "the blocked getUpdates call to arrive")

	cancel()
	close(block)
	if err := <-errc; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := waits.snapshot(); !slices.Equal(got, []time.Duration{7 * time.Second}) {
		t.Errorf("waits %v, want exactly the response's retry_after of 7s", got)
	}
	got := srv.offsetsRecorded()
	if len(got) < 2 || got[0] != "0" || got[1] != "0" {
		t.Errorf("offsets %q, want the repoll to request offset 0 again: backpressure does not advance the offset", got)
	}
	if got := h.forwarded(); !slices.Equal(got, []string{"hello"}) {
		t.Errorf("forwarded %q, want the post-backpressure update", got)
	}
}

func TestPollStallWarnsOnceAtThreshold(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	// Five proven-transient 5xx failures, then a successful long poll: the
	// production HTTP client (serverStatusTransport) is in the loop's path.
	var entries []pollEntry
	for range 5 {
		entries = append(entries, pollEntry{err: botErrorBody(http.StatusInternalServerError, "boom")})
	}
	entries = append(entries, pollEntry{batch: []string{textUpdateJSON(1, 42, 42, "private", "hello")}})
	// Block the call after the success: index 5 must deliver its batch.
	srv := newPollServer(t, entries, map[int]<-chan struct{}{6: block})

	waits := &waitsRecord{}
	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, waits.wait, log)
	p.url = srv.server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := runPoll(p, ctx)
	waitFor(t, 5*time.Second, func() bool { return h.count() == 1 }, "the post-stall update to reach the handler")
	waitFor(t, 5*time.Second, func() bool { return len(srv.offsetsRecorded()) == 7 }, "the blocked getUpdates call to arrive")

	cancel()
	close(block)
	if err := <-errc; err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := capture.countMsg("polling stall"); n != 1 {
		t.Errorf("stall records = %d, want 1 at the threshold", n)
	}
	got := waits.snapshot()
	if len(got) != 5 {
		t.Fatalf("waits = %v, want one per recoverable failure", got)
	}
	for i, d := range got {
		if d != pollBackoff {
			t.Errorf("waits[%d] = %v, want the brief poll backoff %v", i, d, pollBackoff)
		}
	}
}
