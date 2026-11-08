package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

func TestRenderResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		give string
		want string
	}{
		{name: "plain text", give: "hello", want: "hello"},
		{name: "markup characters pass through", give: "use `code` and **bold**", want: "use `code` and **bold**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := renderResponse(tt.give); got != tt.want {
				t.Errorf("renderResponse(%q) = %q, want %q", tt.give, got, tt.want)
			}
		})
	}
}

func TestRenderNoticeWording(t *testing.T) {
	t.Parallel()

	state := func(modelID string, streaming bool) *rpc.State {
		s := &rpc.State{
			SessionID:           "s-123",
			MessageCount:        7,
			PendingMessageCount: 2,
			IsStreaming:         streaming,
		}
		if modelID != "" {
			s.Model = &rpc.Model{ID: modelID}
		}
		return s
	}

	tests := []struct {
		name   string
		notice runtime.Notice
		want   string
	}{
		{
			name:   "status idle",
			notice: runtime.Notice{Kind: runtime.NoticeStatus, State: state("gpt-5", false)},
			want:   "No active operation. Session s-123, 7 messages, model gpt-5.",
		},
		{
			name:   "status idle with nil model renders unknown",
			notice: runtime.Notice{Kind: runtime.NoticeStatus, State: state("", false)},
			want:   "No active operation. Session s-123, 7 messages, model unknown.",
		},
		{
			name:   "status active while streaming",
			notice: runtime.Notice{Kind: runtime.NoticeStatus, State: state("gpt-5", true)},
			want:   "Working. Queue 2. Model gpt-5.",
		},
		{
			name:   "status active with nil model renders unknown",
			notice: runtime.Notice{Kind: runtime.NoticeStatus, State: state("", true)},
			want:   "Working. Queue 2. Model unknown.",
		},
		{
			name:   "steer acknowledged",
			notice: runtime.Notice{Kind: runtime.NoticeSteerAcknowledged},
			want:   "Queued into the current run.",
		},
		{
			name:   "extension handled",
			notice: runtime.Notice{Kind: runtime.NoticeExtensionHandled},
			want:   "Handled by the extension; no run started.",
		},
		{
			name:   "empty response",
			notice: runtime.Notice{Kind: runtime.NoticeEmptyResponse},
			want:   "Pi finished without producing text.",
		},
		{
			name:   "failed run",
			notice: runtime.Notice{Kind: runtime.NoticeFailedRun, Text: "rate limited"},
			want:   "Run failed: rate limited",
		},
		{
			name:   "abort report idle marker",
			notice: runtime.Notice{Kind: runtime.NoticeAbortReport, Text: "no operation was active"},
			want:   "No operation was active.",
		},
		{
			name: "abort report dropped, text never rendered",
			notice: runtime.Notice{
				Kind: runtime.NoticeAbortReport,
				Dropped: rpc.QueueContents{
					Steering: []string{"secret steering"},
					FollowUp: []string{"secret follow-up", "second"},
				},
			},
			want: "Aborted. Dropped 3 queued inputs.",
		},
		{
			name:   "new session report cancelled marker",
			notice: runtime.Notice{Kind: runtime.NoticeNewSessionReport, Text: "session switch cancelled"},
			want:   "Session switch cancelled; current session kept.",
		},
		{
			name: "new session report dropped, text never rendered",
			notice: runtime.Notice{
				Kind:    runtime.NoticeNewSessionReport,
				Dropped: rpc.QueueContents{FollowUp: []string{"pending question"}},
			},
			want: "New session. Dropped 1 queued inputs.",
		},
		{
			name:   "new session report clean",
			notice: runtime.Notice{Kind: runtime.NoticeNewSessionReport},
			want:   "New session started.",
		},
		{
			name:   "session resume failed",
			notice: runtime.Notice{Kind: runtime.NoticeSessionResumeFailed, Text: "bad session file"},
			want:   "Could not resume the last session (bad session file); started fresh.",
		},
		{
			name: "heartbeat",
			notice: func() runtime.Notice {
				n := runtime.Notice{Kind: runtime.NoticeHeartbeat}
				n.Elapsed = 2*time.Minute + 30*time.Second
				return n
			}(),
			want: "Still working — the run started 2m30s ago.",
		},
		{
			name:   "routing failure",
			notice: runtime.Notice{Kind: runtime.NoticeRoutingFailure, Text: "no prompt response"},
			want:   "Trygalle routing failed: no prompt response. The prior input was not lost mid-run; recheck with /status.",
		},
		{
			name:   "restart notice with empty text",
			notice: runtime.Notice{Kind: runtime.NoticeRestartNotice},
			want:   "Restarting; active operation aborted.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := renderNotice(tt.notice)
			if got != tt.want {
				t.Errorf("renderNotice() =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}

func TestRenderNoticeDroppedContentNeverLeaks(t *testing.T) {
	t.Parallel()
	notice := runtime.Notice{
		Kind: runtime.NoticeNewSessionReport,
		Dropped: rpc.QueueContents{
			Steering: []string{"classified steering text"},
			FollowUp: []string{"classified follow-up text"},
		},
	}
	got := renderNotice(notice)
	if strings.Contains(got, "classified") {
		t.Errorf("renderNotice() leaked queue contents: %q", got)
	}
}
