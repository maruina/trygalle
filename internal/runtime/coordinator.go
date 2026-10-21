package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
)

// Session event types the coordinator consumes for state transitions. All
// other state events are metadata only.
const (
	eventAgentStart   = "agent_start"
	eventAgentEnd     = "agent_end"
	eventAgentSettled = "agent_settled"
	eventTurnEnd      = "turn_end"
	eventMessageEnd   = "message_end"
)

// stopReasonError marks a failed assistant message (rpc.md message types).
const stopReasonError = "error"

const roleAssistant = "assistant"

// queryTimeout bounds the get_last_assistant_text call the loop makes after a
// run settles. deliberate: the value is not pinned by the plan; a stuck query
// would otherwise hold the whole event loop. The client's died channel still
// unblocks the call when Pi dies.
const queryTimeout = 30 * time.Second

// opState is the coordinator operation state machine (design "Operation state
// machine"). Pi remains the authority on the underlying run; the state tracks
// operations for attribution and visibility only.
type opState string

const (
	stateIdle       opState = "idle"
	stateRunning    opState = "running"
	stateSuperseded opState = "superseded"
)

// operation tracks the run started by one user input plus everything steered
// into it. All fields are owned by the coordinator event loop goroutine.
type operation struct {
	hasRun        bool // an agent_start was observed for this operation
	settled       bool // agent_settled was observed for this operation
	startedAt     time.Time
	lastAssistant *rpc.Message // last assistant message observed, for failure classification
}

var (
	errCoordinatorClosed = errors.New("coordinator is stopped")
	errInputInFlight     = errors.New("another input is already in flight")
	errNoPendingInput    = errors.New("no input in flight")
)

// cmdKind discriminates the internal command channel from HandleUserInput to
// the coordinator event loop. The loop serializes every state transition, and
// the ack wait orders the RPC outcome against the event stream.
type cmdKind int

const (
	cmdBegin  cmdKind = iota // an input is about to be submitted to Pi
	cmdResult                // the RPC outcome of a submission
)

type coordCommand struct {
	kind        cmdKind
	op          *operation // the submission this begin or result belongs to
	disposition Disposition
	err         error
	ack         chan error
}

// Coordinator routes user inputs to Pi and owns the operation state machine.
// One long-lived event loop is the sole consumer of the client's Events() and
// UIRequests() channels, observes Pi process exit through one bounded Wait
// goroutine, and serializes all state transitions. HandleUserInput returns
// after the RPC response, not after operation settlement, so the caller can
// route further input during a run.
//
// HandleUserInput expects a single sequential caller (the REPL reads stdin
// line by line); concurrent callers get errInputInFlight.
type Coordinator struct {
	client   PiClient
	notifier Notifier
	cfg      Config
	log      *slog.Logger

	commands  chan coordCommand
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// fatalError is written once by the event loop before done closes, so
	// readers that observed done closed can read it safely.
	fatalError error

	// op, pending, state, and the heartbeat ticker are owned by the event
	// loop goroutine.
	op      *operation // the running operation, non-nil while stateRunning
	pending *operation // a submit between the begin and result handshakes
	state   opState

	hbTicker *time.Ticker // non-nil while an operation runs
	hbC      <-chan time.Time
}

// NewCoordinator starts the coordinator event loop. Close stops it.
func NewCoordinator(client PiClient, notifier Notifier, cfg Config, log *slog.Logger) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	c := &Coordinator{
		client:   client,
		notifier: notifier,
		cfg:      cfg,
		log:      log,
		commands: make(chan coordCommand),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.loop()
	return c
}

// Close stops the coordinator event loop. It does not terminate the Pi
// process; the caller owns shutdown (see the shutdown design section).
func (c *Coordinator) Close() {
	c.closeOnce.Do(func() { close(c.stop) })
}

// Done closes when the coordinator stops.
// HandleUserInput routes one user input. Reserved commands are recognized but
// routed in Slice 4; everything else goes to prompt with streamingBehavior
// "steer", in every state: Pi decides between started, queued, and handled
// because Pi is the authority on run state. The method returns after the RPC
// response, or with the coordinator's fatal error once Pi is unusable.
func (c *Coordinator) HandleUserInput(ctx context.Context, text string) error {
	if isReservedCommand(text) {
		// Reserved commands never reach prompt{steer}. Their routing
		// (/status, /abort, /new) lands in Slice 4.
		c.log.Info("reserved command; routing lands in slice 4", "kind", text)
		return nil
	}

	// submit identifies this handshake, so a late or compensating result
	// command can only clear the pending submit it belongs to.
	submit := &operation{}
	if err := c.sendCommand(ctx, coordCommand{kind: cmdBegin, op: submit}); err != nil {
		if ctx.Err() != nil {
			// The caller gave up after the begin may have registered the
			// submit. Clear it so a later input is not held hostage by it;
			// the loop ignores the command when submit is no longer pending.
			_ = c.sendCommand(context.Background(), coordCommand{kind: cmdResult, op: submit, err: ctx.Err()})
		}
		return err
	}

	disposition, err := c.client.Prompt(ctx, text)
	if err != nil && isCommandRejection(err) {
		// prompt rejects input while compaction is in progress, while steer
		// queues during it; retry once with bare steer so the queue stays
		// with Pi and Trygalle stays stateless (design "Message routing").
		// Only an explicit Pi rejection retries: a transport failure or a
		// canceled caller may already have delivered the prompt.
		c.log.Info("prompt rejected; retrying once with bare steer", "error", err)
		disposition, err = c.client.Steer(ctx, text)
	}
	// The result handshake uses a coordinator-owned context: the loop must
	// learn the outcome even when the caller gave up, or pending state strands
	// and every later input fails errInputInFlight.
	return c.sendResult(context.Background(), submit, disposition, err)
}

// isCommandRejection reports whether Pi explicitly rejected the prompt, the
// only failure for which a retry cannot duplicate a delivered message.
func isCommandRejection(err error) bool {
	var cmdErr *rpc.CommandError
	return errors.As(err, &cmdErr)
}

func (c *Coordinator) sendResult(ctx context.Context, op *operation, d Disposition, err error) error {
	return c.sendCommand(ctx, coordCommand{kind: cmdResult, op: op, disposition: d, err: err})
}

// sendCommand hands one internal command to the event loop and waits for its
// ack, the coordinator's terminal condition, or ctx cancellation.
func (c *Coordinator) sendCommand(ctx context.Context, cmd coordCommand) error {
	if cmd.ack == nil {
		cmd.ack = make(chan error, 1)
	}
	select {
	case c.commands <- cmd:
		select {
		case err := <-cmd.ack:
			return err
		case <-c.done:
			return c.terminalError()
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-c.done:
		return c.terminalError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Coordinator) terminalError() error {
	if c.fatalError != nil {
		return c.fatalError
	}
	return errCoordinatorClosed
}

// isReservedCommand reports whether text is one of the exact-match reserved
// command names (plan "REPL entry seam").
func isReservedCommand(text string) bool {
	switch text {
	case "/status", "/abort", "/new":
		return true
	default:
		return false
	}
}

// loop is the single coordinator event loop. It owns operation state,
// consumes the client's event and UI request streams, and observes process
// exit. All state transitions happen in this goroutine.
func (c *Coordinator) loop() {
	defer close(c.done)

	// One bounded Wait goroutine observes pi exit. Wait caches its result, so
	// every observer sees the same terminal condition. The exit channel is
	// buffered because after the loop stops, nobody drains it.
	exitCh := make(chan error, 1)
	go func() { exitCh <- c.client.Wait() }()

	for {
		select {
		case cmd := <-c.commands:
			switch cmd.kind {
			case cmdBegin:
				c.handleBegin(cmd)
			case cmdResult:
				c.handleResult(cmd)
			}
		case e := <-c.client.Events():
			c.handleEvent(e)
		case <-c.hbC:
			c.heartbeatTick()
		case u := <-c.client.UIRequests():
			c.handleUIRequest(u)
		case err := <-exitCh:
			switch {
			case err == nil:
				// An orderly exit only follows a stdin close, which the
				// coordinator never requests while running; treat it as
				// an unexpected exit.
				c.fail(errors.New("pi exited unexpectedly without a failure"))
			default:
				c.fail(fmt.Errorf("pi failed: %w", err))
			}
			c.stopHeartbeat()
			return
		case <-c.stop:
			c.stopHeartbeat()
			return
		}
	}
}

// fail records the terminal condition and ends the loop; the deferred
// close(done) publishes it to every waiter.
func (c *Coordinator) fail(err error) {
	c.fatalError = err
	c.log.Error("coordinator fatal", "error", err)
}

func (c *Coordinator) handleBegin(cmd coordCommand) {
	if c.pending != nil {
		cmd.ack <- errInputInFlight
		return
	}
	c.pending = cmd.op
	cmd.ack <- nil
}

// drainEvents processes every event already enqueued by the client, without
// blocking. The client enqueues an event before the response of any record Pi
// emitted after the event, so an agent_start delivered before a response is
// in the channel buffer when the response arrives; this closes the only
// remaining scheduling race between the events and command channels.
func (c *Coordinator) drainEvents() {
	for {
		select {
		case e := <-c.client.Events():
			c.handleEvent(e)
		default:
			return
		}
	}
}

// startOperation promotes the pending submit into the tracked operation. It
// reports false when an operation already runs (Pi would have queued, not
// started); the caller still gets a clean ack and the state stays honest.
func (c *Coordinator) startOperation(pending *operation) bool {
	if c.op != nil {
		c.log.Error("prompt started a second run while an operation is running")
		return false
	}
	pending.startedAt = time.Now()
	c.op = pending
	c.state = stateRunning
	c.startHeartbeat()
	c.log.Info("operation started")
	return true
}

func (c *Coordinator) settleCurrentOperation() {
	op := c.op
	if op == nil {
		return
	}
	c.op = nil
	c.state = stateIdle
	c.stopHeartbeat()
	c.log.Info("operation settled")
	c.settleOperation(op)
}

// settleOperation produces the visible outcome of one settled run (design
// "Terminal states"): a failed run reports Pi's error message, otherwise the
// final text is queried from Pi, the authority on session content.
func (c *Coordinator) settleOperation(op *operation) {
	if op.lastAssistant != nil && op.lastAssistant.StopReason == stopReasonError {
		c.log.Info("operation failed", "stopReason", op.lastAssistant.StopReason)
		c.notifier.Notice(context.Background(), Notice{
			Kind: NoticeFailedRun,
			Text: op.lastAssistant.ErrorMessage,
		})
		return
	}
	text, err := c.lastAssistantText()
	switch {
	case err != nil:
		// deliberate: get_last_assistant_text over the local pipe only fails
		// when the protocol died; the process-exit path then takes over.
		// RoutingFailure keeps the outcome visible until the Telegram design
		// pins a notice kind for a failed final-text query.
		c.log.Error("query last assistant text failed", "error", err)
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeRoutingFailure, Text: err.Error()})
	case text == nil || *text == "":
		// An empty string is no text, same as JSON null (design "Terminal
		// states").
		c.notifier.Notice(context.Background(), Notice{Kind: NoticeEmptyResponse})
	default:
		c.notifier.Response(context.Background(), *text)
	}
}

func (c *Coordinator) lastAssistantText() (*string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	return c.client.GetLastAssistantText(ctx)
}

// handleEvent classifies one session event. Attribution rule: events attach
// to the running operation, else to the pending submit, else they are logged
// as unassociated and never attributed (design "Operation state machine").
func (c *Coordinator) handleEvent(e Event) {
	switch e.Type {
	case eventAgentStart:
		c.noteAgentStart()
	case eventAgentSettled:
		c.noteSettled()
	case eventMessageEnd, eventTurnEnd:
		c.noteMessage(e.Message)
	case eventAgentEnd:
		for i := range e.Messages {
			c.noteMessage(&e.Messages[i])
		}
		if e.WillRetry {
			c.log.Info("agent ended; pi will retry")
		}
	default:
		// Compaction, retry, and queue events: metadata only, no content.
		c.log.Debug("session event", "type", e.Type)
	}
}

func (c *Coordinator) noteAgentStart() {
	switch {
	case c.op != nil:
		c.op.hasRun = true
	case c.pending != nil:
		c.pending.hasRun = true
	default:
		c.log.Debug("agent_start outside any tracked operation")
	}
}

func (c *Coordinator) noteSettled() {
	switch {
	case c.op != nil:
		c.settleCurrentOperation()
	case c.pending != nil:
		c.pending.settled = true
	default:
		// agent_settled without a tracked operation: logged, not attributed.
		// Mid-run supersede trailing events land with /new (Slice 4).
		c.log.Debug("agent_settled outside any tracked operation")
	}
}

// noteMessage tracks the last assistant message for terminal-state
// classification. Content is dropped; only role, stopReason, and the error
// message are retained.
func (c *Coordinator) noteMessage(m *rpc.Message) {
	if m == nil || m.Role != roleAssistant {
		return
	}
	target := c.op
	if target == nil {
		target = c.pending
	}
	if target == nil {
		return
	}
	target.lastAssistant = m
}

// handleUIRequest answers every blocking extension dialog immediately with
// cancelled: true, so an ignored dialog can never block a run forever; a
// dialog the run does not need behaves like Esc (design "Extension UI").
// Fire-and-forget requests are logged as metadata. The dedicated policy tests
// land with Task 16 (Slice 5).
func (c *Coordinator) handleUIRequest(u UIRequest) {
	switch u.Method {
	case "select", "confirm", "input", "editor":
		c.log.Info("cancelling extension dialog", "method", u.Method)
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		defer cancel()
		if err := c.client.AnswerUIDialog(ctx, u.ID); err != nil {
			c.log.Error("answer extension dialog", "error", err)
		}
	default:
		c.log.Info("extension ui notification", "method", u.Method)
	}
}
