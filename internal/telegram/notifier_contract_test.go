package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	botmodels "github.com/go-telegram/bot/models"
	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// The transport's contract is the runtime.Notifier seam driven like the
// coordinator drives it: calls return promptly, outbound text matches the
// render boundary, and every send targets the allowed chat.

func contractSendOK(_ context.Context, _ int, _ *bot.SendMessageParams) (*botmodels.Message, error) {
	return okSend()
}

func TestTransportContractResponsePassthrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)
	tt.Response(ctx, "use `code` and **bold**")
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 1 }, "one delivery")
	if got := s.delivered()[0]; got != "use `code` and **bold**" {
		t.Errorf("delivered %q, want the response as received", got)
	}
}

func TestTransportContractNoticeRouting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)

	notices := []runtime.Notice{
		{Kind: runtime.NoticeStatus, State: &rpc.State{SessionID: "s-1", MessageCount: 2}},
		{Kind: runtime.NoticeSteerAcknowledged},
		{Kind: runtime.NoticeExtensionHandled},
		{Kind: runtime.NoticeEmptyResponse},
		{Kind: runtime.NoticeFailedRun, Text: "boom"},
		{Kind: runtime.NoticeAbortReport, Dropped: rpc.QueueContents{FollowUp: []string{"x"}}},
		{Kind: runtime.NoticeNewSessionReport},
		{Kind: runtime.NoticeSessionResumeFailed, Text: "bad file"},
		{Kind: runtime.NoticeHeartbeat, Elapsed: time.Minute},
		{Kind: runtime.NoticeRoutingFailure, Text: "no prompt"},
		{Kind: runtime.NoticeRestartNotice},
	}
	for _, n := range notices {
		tt.Notice(ctx, n)
	}
	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == len(notices) }, "one delivery per notice kind")
	if got := s.chatID(); got != 42 {
		t.Errorf("chat id %d, want the allowed user", got)
	}
	last := s.delivered()[len(s.delivered())-1]
	if last != "Restarting; active operation aborted." {
		t.Errorf("restart notice delivered %q, want the pinned wording", last)
	}
}

func TestTransportContractChunkedResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)

	give := strings.Repeat("a", maxMessageUTF16CodeUnits+20) + "\nline break\n" + strings.Repeat("b", 10)
	tt.Response(ctx, give)

	waitFor(t, 2*time.Second, func() bool { return len(s.delivered()) == 2 }, "the chunk count through Notifier")
	got := s.delivered()
	if strings.Join(got, "") != give {
		t.Errorf("chunks reassembled into different text")
	}
	for i, c := range got {
		if units := utf16StringLen(c); units > maxMessageUTF16CodeUnits {
			t.Errorf("chunk %d has %d units, over the limit", i, units)
		}
	}
}

func TestWorkingNoticeIsATypingPulseNeverAMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)

	for range 5 {
		tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeWorking})
	}
	waitFor(t, 2*time.Second, func() bool { return s.chatActionCount() >= 1 }, "the first typing pulse")
	time.Sleep(50 * time.Millisecond)
	if n := s.chatActionCount(); n < 1 {
		t.Errorf("typing pulses = %d, want at least 1", n)
	}
	if got := len(s.delivered()); got != 0 {
		t.Errorf("typing pulses delivered %d chat messages, want 0", got)
	}
	if got := s.chatActionID(); got != 42 {
		t.Errorf("typing pulse chat = %d, want the allowed user's chat", got)
	}
}

// fastTypingTimers shrinks one transport's typing cadence to a millisecond
// scale; the per-transport fields keep parallel tests race-free.
func fastTypingTimers(tt *Transport) {
	tt.typingTimeout = 50 * time.Millisecond
	tt.typingRefresh = 30 * time.Millisecond
	tt.typingRefreshWindow = 120 * time.Millisecond
}

func TestTypingScopeRefreshesWhileActivityContinues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)
	fastTypingTimers(tt)

	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeWorking})
	// One send on entry, then refreshes every cadence while the scope is
	// live and no later milestone needs to re-pulse.
	waitFor(t, 2*time.Second, func() bool { return s.chatActionCount() >= 3 }, "the refresh cadence to keep the indicator alive")
	if got := len(s.delivered()); got != 0 {
		t.Errorf("typing refreshes delivered %d chat messages, want 0", got)
	}
}

func TestTypingScopeIdlesAfterSilence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)
	fastTypingTimers(tt)

	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeWorking})
	waitFor(t, 2*time.Second, func() bool { return s.chatActionCount() >= 1 }, "the first typing send")
	// The window lapses with no further pulses: the scope idles and the
	// count freezes instead of looping forever.
	time.Sleep(3 * tt.typingRefreshWindow)
	frozen := s.chatActionCount()
	time.Sleep(50 * time.Millisecond)
	if got := s.chatActionCount(); got != frozen {
		t.Errorf("typing sends after the silent window = %d, frozen at %d", got, frozen)
	}
}

func TestTypingSuppressesAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt, s := newStubTransport(t, nil, nil, contractSendOK)
	fastTypingTimers(tt)
	s.chatActionErr = errors.New("chat action rejected")

	tt.Notice(ctx, runtime.Notice{Kind: runtime.NoticeWorking})
	time.Sleep(3 * tt.typingRefreshWindow)
	first := s.chatActionCount()
	if first == 0 {
		t.Fatal("no chat-action attempts observed")
	}
	if first > tt.typingSuppressAfter {
		t.Errorf("chat-action attempts in one failing scope = %d, want suppression after %d", first, tt.typingSuppressAfter)
	}
	frozen := s.chatActionCount()
	time.Sleep(50 * time.Millisecond)
	if got := s.chatActionCount(); got != frozen {
		t.Errorf("attempts after suppression = %d, frozen at %d", got, frozen)
	}
}
