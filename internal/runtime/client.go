package runtime

import (
	"context"

	"github.com/maruina/trygalle/internal/rpc"
)

// Aliases pin the runtime seam to the protocol record types without a "pi"
// prefix in this package's API (plan "Key Decisions").
type (
	// Disposition is the outcome of a prompt or steer command.
	Disposition = rpc.Disposition
	// State is the data of a get_state response.
	State = rpc.State
	// CommandInfo is one entry of a get_commands response.
	CommandInfo = rpc.CommandInfo
	// QueueContents holds the steering and follow-up texts Pi reports for a
	// clear_queue response.
	QueueContents = rpc.QueueContents
	// Event is one state event from the session event stream.
	Event = rpc.Event
	// Activity is the most recent event of any type the client observed.
	Activity = rpc.Activity
	// UIRequest is one extension UI record from Pi.
	UIRequest = rpc.UIRequest
)

// Disposition values re-exported for routing without importing rpc at call
// sites that only compare dispositions.
const (
	DispositionStarted = rpc.DispositionStarted
	DispositionQueued  = rpc.DispositionQueued
	DispositionHandled = rpc.DispositionHandled
)

// PiClient is the seam between the coordinator and the RPC client. The
// coordinator consumes it; unit tests use a scripted fake.
type PiClient interface {
	Prompt(ctx context.Context, message string) (Disposition, error) // prompt with streamingBehavior "steer"
	Steer(ctx context.Context, message string) (Disposition, error)
	GetState(ctx context.Context) (State, error)
	GetCommands(ctx context.Context) ([]CommandInfo, error)
	Abort(ctx context.Context) error
	ClearQueue(ctx context.Context) (QueueContents, error)
	NewSession(ctx context.Context) (cancelled bool, err error)
	GetLastAssistantText(ctx context.Context) (*string, error) // nil pointer == JSON null
	Events() <-chan Event                                      // state events only; one coordinator consumer
	LastActivity() Activity                                    // type and arrival time of the most recent event of any type
	UIRequests() <-chan UIRequest                              // one coordinator consumer
	AnswerUIDialog(ctx context.Context, id string) error       // extension_ui_response with cancelled: true
	CloseStdin() error
	Kill() error // terminate a child that misses its bounded graceful-exit deadline
	Wait() error // waits for exit; all callers observe the same cached result
}

// Compile-time assertion: the real RPC client satisfies the seam.
var _ PiClient = (*rpc.Client)(nil)
