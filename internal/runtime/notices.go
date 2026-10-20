package runtime

import (
	"context"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
)

// NoticeKind is the category of a system notice. Wording is owned by the
// Telegram design; this layer carries structured data only.
type NoticeKind string

// The notice kinds of the pinned notifier seam. ExtensionHandled extends the
// original ten with the outcome of an extension command that consumed a
// mid-run input (see the coordinator handled-mid-run branch).
const (
	NoticeStatus              NoticeKind = "Status"
	NoticeSteerAcknowledged   NoticeKind = "SteerAcknowledged"
	NoticeExtensionHandled    NoticeKind = "ExtensionHandled"
	NoticeEmptyResponse       NoticeKind = "EmptyResponse"
	NoticeFailedRun           NoticeKind = "FailedRun"
	NoticeAbortReport         NoticeKind = "AbortReport"
	NoticeNewSessionReport    NoticeKind = "NewSessionReport"
	NoticeSessionResumeFailed NoticeKind = "SessionResumeFailed"
	NoticeHeartbeat           NoticeKind = "Heartbeat"
	NoticeRoutingFailure      NoticeKind = "RoutingFailure"
	NoticeRestartNotice       NoticeKind = "RestartNotice"
)

// Notice is one system notice: a category plus the small structured payload
// the category carries. Zero fields are omitted by the kinds that do not use
// them, so every kind has one obvious payload.
type Notice struct {
	Kind NoticeKind

	// Text is the kind's free-text payload: Pi's error message for
	// FailedRun, the failure cause for RoutingFailure and RestartNotice,
	// and the resume-failure cause for SessionResumeFailed.
	Text string

	// State is the Status payload (Slice 4).
	State *rpc.State

	// Dropped carries the steering and follow-up texts Pi removed from its
	// queue for AbortReport and NewSessionReport (Slice 4).
	Dropped rpc.QueueContents

	// Elapsed and LastActivity are the Heartbeat payload. LastActivity is
	// metadata only (event type and arrival time); its zero value means no
	// event was observed yet.
	Elapsed      time.Duration
	LastActivity rpc.Activity
}

// Notifier receives Pi responses and system notices. The REPL implements it
// with stdout in this slice; the Telegram transport implements the same seam
// later (follow-up #2).
type Notifier interface {
	Response(ctx context.Context, text string)
	Notice(ctx context.Context, notice Notice)
}
