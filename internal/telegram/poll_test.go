package telegram

import (
	"context"
	"errors"
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
	srv := newPollServer(t, [][]string{{
		textUpdateJSON(1, 42, 42, "private", "hello"),
		textUpdateJSON(2, 42, 42, "private", "/status"),
		textUpdateJSON(3, 42, 42, "private", "steer this"),
	}}, map[int]<-chan struct{}{1: block})

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, log)
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
	srv := newPollServer(t, [][]string{{
		textUpdateJSON(1, 999, 42, "private", "stranger text"),
		textUpdateJSON(2, 42, 42, "group", "group text"),
		voiceUpdateJSON(3, 999, 42, "private"),
		textUpdateJSON(4, 42, 42, "private", "allowed"),
	}}, map[int]<-chan struct{}{1: block})

	capture, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, log)
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
	srv := newPollServer(t, [][]string{{
		voiceUpdateJSON(1, 42, 42, "private"),
		callbackUpdateJSON(2, 42),
	}}, map[int]<-chan struct{}{1: block})

	_, log := newCapture()
	h := &fakeHandler{}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, log)
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
	srv := newPollServer(t, [][]string{{
		textUpdateJSON(5, 42, 42, "private", "one"),
		textUpdateJSON(6, 42, 42, "private", "two"),
	}}, nil)

	_, log := newCapture()
	h := &fakeHandler{err: sentinel}
	p := newPoller(validatedConfig(t), newBotAPI(t, srv.server.URL), h, log)
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
