package telegram

import (
	"fmt"

	"github.com/maruina/trygalle/internal/rpc"
	"github.com/maruina/trygalle/internal/runtime"
)

// Notice wording constants. The pinned wording table lives in the design
// (plans/telegram-interface/design.md) and is pinned by the render tests;
// changing wording touches exactly those tests.
const (
	abortIdleMarker        = "no operation was active"
	sessionCancelledMarker = "session switch cancelled"
)

// renderResponse returns Pi's final text as received. The beta dialect is
// plain text with no parse_mode, so no escape state exists.
func renderResponse(text string) string {
	return text
}

// renderNotice returns the outbound text for one notice kind per the pinned
// wording table. Dropped queue contents are never rendered: only their count
// contributes, so steering and follow-up texts stay out of the chat.
func renderNotice(n runtime.Notice) string {
	switch n.Kind {
	case runtime.NoticeStatus:
		return renderStatus(n.State)
	case runtime.NoticeSteerAcknowledged:
		return "Queued into the current run."
	case runtime.NoticeExtensionHandled:
		return "Handled by the extension; no run started."
	case runtime.NoticeEmptyResponse:
		return "Pi finished without producing text."
	case runtime.NoticeFailedRun:
		return "Run failed: " + n.Text
	case runtime.NoticeAbortReport:
		if n.Text == abortIdleMarker {
			return "No operation was active."
		}
		return fmt.Sprintf("Aborted. Dropped %d queued inputs.", droppedCount(n.Dropped))
	case runtime.NoticeNewSessionReport:
		if n.Text == sessionCancelledMarker {
			return "Session switch cancelled; current session kept."
		}
		if count := droppedCount(n.Dropped); count > 0 {
			return fmt.Sprintf("New session. Dropped %d queued inputs.", count)
		}
		return "New session started."
	case runtime.NoticeSessionResumeFailed:
		return fmt.Sprintf("Could not resume the last session (%s); started fresh.", n.Text)
	case runtime.NoticeHeartbeat:
		return fmt.Sprintf("Still working — the run started %s ago.", n.Elapsed)
	case runtime.NoticeRoutingFailure:
		return "Trygalle routing failed: " + n.Text +
			". The prior input was not lost mid-run; recheck with /status."
	case runtime.NoticeRestartNotice:
		return "Restarting; active operation aborted."
	default:
		// The notice kinds are a closed set on the pinned seam; a new kind
		// must land here. Fail loud instead of silently sending garbage.
		return fmt.Sprintf("Trygalle notice: %s.", n.Kind)
	}
}

// renderStatus renders the Status notice from rpc.State. Idle and active
// discriminate on State.IsStreaming.
// deliberate: a run in its pre-streaming phase renders the idle wording,
// pinned by the alignment brief (decision 2); Heartbeat already covers
// elapsed liveness while a run is active.
func renderStatus(state *rpc.State) string {
	modelID := "unknown"
	if state.Model != nil {
		modelID = state.Model.ID
	}
	if state.IsStreaming {
		return fmt.Sprintf("Working. Queue %d. Model %s.", state.PendingMessageCount, modelID)
	}
	return fmt.Sprintf("No active operation. Session %s, %d messages, model %s.",
		state.SessionID, state.MessageCount, modelID)
}

// droppedCount is the only way a Dropped payload contributes: as a count.
// Dropped is a value struct that is always present (rpc.QueueContents), so
// emptiness, not presence, is the signal.
func droppedCount(dropped rpc.QueueContents) int {
	return len(dropped.Steering) + len(dropped.FollowUp)
}
