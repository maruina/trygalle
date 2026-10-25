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
	errReservedInFlight  = errors.New("a reserved command is already in flight")
)

// cmdKind discriminates the internal command channel from HandleUserInput to
// the coordinator event loop. The loop serializes every state transition, and
// the ack wait orders the RPC outcome against the event stream.
type cmdKind int

const (
	cmdBegin    cmdKind = iota // an input is about to be submitted to Pi
	cmdResult                  // the RPC outcome of a submission
	cmdStatus                  // a /status query outcome; read-only, no operation state
	cmdAbort                   // a /abort sequence outcome; terminates the running operation
	cmdNew                     // a /new sequence outcome; supersedes the running operation
	cmdReserve                 // opens the reserved-command window the caller's RPC sequence runs in
	cmdShutdown                // the process is shutting down; no terminal outcome is attributed after it
)

type coordCommand struct {
	kind        cmdKind
	op          *operation // the submission this begin or result belongs to
	disposition Disposition
	status      State         // the /status query outcome for cmdStatus
	dropped     QueueContents // the clear_queue outcome for cmdAbort and cmdNew
	cancelled   bool          // the new_session outcome for cmdNew
	running     chan bool     // the active-operation answer for cmdShutdown
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

	// op, pending, state, the reserved-command window, and the heartbeat
	// ticker are owned by the event loop goroutine.
	op      *operation // the running operation, non-nil while stateRunning
	pending *operation // a submit between the begin and result handshakes
	state   opState

	// reserved is set from the reserve command until the /abort or /new
	// outcome arrives, that is, while the caller runs its RPC sequence. A run
	// that settles inside the window has its terminal outcome deferred, so the
	// reserved command's report is the operation's single terminal outcome
	// (design "Terminal states").
	reserved      bool
	reservedHadOp bool       // an operation was active when the window opened
	deferred      *operation // the operation that settled inside the window

	// shutdownRequested is set when Shutdown takes over. From then on a clean
	// pi exit is the expected end of the process, and run outcomes are not
	// attributed because the shutdown sequence owns them (R17).
	shutdownRequested bool

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
// HandleUserInput routes one user input. /status runs a read-only get_state
// query; /abort runs clear_queue then abort; /new runs clear_queue then
// new_session and supersedes the active run; everything
// else goes to prompt with streamingBehavior
// "steer", in every state: Pi decides between started, queued, and handled
// because Pi is the authority on run state. The method returns after the RPC
// response, or with the coordinator's fatal error once Pi is unusable.
func (c *Coordinator) HandleUserInput(ctx context.Context, text string) error {
	if text == "/status" {
		return c.handleStatusRequest(ctx)
	}
	if text == "/abort" {
		return c.handleAbortRequest(ctx)
	}
	if text == "/new" {
		return c.handleNewRequest(ctx)
	}
	if isReservedCommand(text) {
		// Defensive: every reserved name routes above. This branch only
		// fires if a new reserved name is added to the classifier first.
		c.log.Info("reserved command routed above", "kind", text)
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

// handleAbortRequest routes /abort with Esc parity: open the reserved-command
// window, clear the steering queue before aborting the run, then post the
// sequence outcome so the loop alone emits the AbortReport as the operation's
// single terminal outcome (R12). An idle /abort still runs the sequence,
// mirroring Esc on an idle session.
func (c *Coordinator) handleAbortRequest(ctx context.Context) error {
	if err := c.sendCommand(ctx, coordCommand{kind: cmdReserve}); err != nil {
		return c.closeAbandonedWindow(ctx, err, cmdAbort)
	}
	dropped, err := c.clearQueueAndAbort(ctx)
	// The handoff uses a coordinator-owned context so a caller that gave up
	// still leaves a visible outcome, mirroring the prompt result handshake.
	return c.sendCommand(context.Background(), coordCommand{kind: cmdAbort, dropped: dropped, err: err})
}

// closeAbandonedWindow closes the reserved-command window when the caller gave
// up after the reserve may have reached the loop. The outcome command carries
// the cancellation, so the window never strands the suppression it owns.
func (c *Coordinator) closeAbandonedWindow(ctx context.Context, err error, kind cmdKind) error {
	if ctx.Err() == nil {
		return err
	}
	_ = c.sendCommand(context.Background(), coordCommand{kind: kind, err: err})
	return err
}

// clearQueueAndAbort runs the Esc-parity sequence: drain queued steering and
// follow-up messages first so nothing survives the abort, then abort the
// run. It short-circuits on the first failure; the loop reports it and the
// operation (if any) keeps running so its settle is still attributed.
func (c *Coordinator) clearQueueAndAbort(ctx context.Context) (QueueContents, error) {
	dropped, err := c.client.ClearQueue(ctx)
	if err != nil {
		return QueueContents{}, err
	}
	if err := c.client.Abort(ctx); err != nil {
		return dropped, err
	}
	return dropped, nil
}

// handleNewRequest routes /new with immediate-execution semantics: clear the
// steering queue, then switch to a new session, then post the sequence
// outcome so the loop alone owns the supersede transition (R13).
func (c *Coordinator) handleNewRequest(ctx context.Context) error {
	if err := c.sendCommand(ctx, coordCommand{kind: cmdReserve}); err != nil {
		return c.closeAbandonedWindow(ctx, err, cmdNew)
	}
	dropped, cancelled, err := c.clearQueueAndNewSession(ctx)
	// The handoff uses a coordinator-owned context so a caller that gave up
	// still leaves a visible outcome, mirroring the prompt result handshake.
	return c.sendCommand(context.Background(), coordCommand{kind: cmdNew, dropped: dropped, cancelled: cancelled, err: err})
}

// clearQueueAndNewSession runs the immediate-execution sequence: drain queued
// steering and follow-up messages first so nothing survives the switch, then
// start a new session (an extension handler can still cancel it). It
// short-circuits on the first failure; the loop reports it and the operation
// (if any) keeps running so its settle is still attributed.
func (c *Coordinator) clearQueueAndNewSession(ctx context.Context) (QueueContents, bool, error) {
	dropped, err := c.client.ClearQueue(ctx)
	if err != nil {
		return QueueContents{}, false, err
	}
	cancelled, err := c.client.NewSession(ctx)
	if err != nil {
		return dropped, false, err
	}
	return dropped, cancelled, nil
}

// handleStatusRequest routes /status: a read-only get_state query whose
// outcome the loop reports as a Status notice. It never touches operation
// state and uses no begin/result handshake, so it routes while a run is
// active without disturbing it (R11).
func (c *Coordinator) handleStatusRequest(ctx context.Context) error {
	state, err := c.client.GetState(ctx)
	// The handoff uses a coordinator-owned context so a caller that gave up
	// still leaves a visible outcome, mirroring the prompt result handshake.
	return c.sendCommand(context.Background(), coordCommand{kind: cmdStatus, status: state, err: err})
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
			case cmdStatus:
				c.handleStatus(cmd)
			case cmdAbort:
				c.handleAbort(cmd)
			case cmdNew:
				c.handleNew(cmd)
			case cmdReserve:
				c.handleReserve(cmd)
			case cmdShutdown:
				c.handleShutdown(cmd)
			}
		case e := <-c.client.Events():
			c.handleEvent(e)
		case <-c.hbC:
			c.heartbeatTick()
		case u := <-c.client.UIRequests():
			c.handleUIRequest(u)
		case err := <-exitCh:
			switch {
			case err == nil && c.shutdownRequested:
				// The shutdown sequence closed stdin, so a clean exit is
				// the expected end of the process.
				c.log.Info("pi exited after the shutdown request")
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

// handleReserve opens the reserved-command window and records whether an
// operation was active when it opened, which is what the /abort report needs
// when the operation settles inside the window.
// handleShutdown puts the loop in shutdown mode: a clean pi exit after this
// point is the expected end of the process, not a fatal condition, and a run
// that settles is not attributed a terminal outcome because the shutdown
// sequence owns it. The restart notice is best effort and runs on its own
// goroutine so a slow transport can never delay shutdown (design "Shutdown",
// step 4).
func (c *Coordinator) handleShutdown(cmd coordCommand) {
	c.shutdownRequested = true
	c.log.Info("shutdown requested")
	cmd.running <- c.op != nil
	cmd.ack <- nil
	go c.notifier.Notice(context.Background(), Notice{Kind: NoticeRestartNotice})
}

func (c *Coordinator) handleReserve(cmd coordCommand) {
	if c.reserved {
		cmd.ack <- errReservedInFlight
		return
	}
	c.reserved = true
	c.reservedHadOp = c.op != nil
	cmd.ack <- nil
}

// flushDeferred delivers the terminal outcome of an operation that settled
// inside a reserved-command window. It runs only when that command was
// canceled or failed, so the run's answer is never lost.
func (c *Coordinator) flushDeferred() {
	op := c.deferred
	if op == nil {
		return
	}
	c.deferred = nil
	c.state = stateIdle
	c.log.Info("delivering the outcome deferred by a reserved command")
	c.settleOperation(op)
}

// discardDeferred drops the deferred outcome after a reserved command
// completed: its report is the operation's single terminal outcome.
func (c *Coordinator) discardDeferred() {
	if c.deferred == nil {
		return
	}
	c.deferred = nil
	c.log.Info("discarding the outcome replaced by a reserved command")
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
// A post-/new suppression survives a new operation start: only the next
// agent_start re-arms attribution, so a late settle from the superseded run
// can never complete the new operation (R13).
func (c *Coordinator) startOperation(pending *operation) bool {
	if c.op != nil {
		c.log.Error("prompt started a second run while an operation is running")
		return false
	}
	pending.startedAt = time.Now()
	c.op = pending
	if c.state != stateSuperseded {
		c.state = stateRunning
	}
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
// After /new supersedes the session or /abort ends the operation, trailing
// events from that run are dropped until the next agent_start re-arms
// attribution (R12, R13).
func (c *Coordinator) handleEvent(e Event) {
	if c.state == stateSuperseded && !c.endSupersede(e) {
		// A late settle from the superseded run can never complete a later
		// input: it is logged with its type and dropped here.
		c.log.Debug("trailing event after supersede", "type", e.Type)
		return
	}
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

// endSupersede reports whether e ends the post-terminal trailing-event
// suppression. Only the next agent_start re-arms attribution: it must belong
// to a new run because the superseded or aborted run already started. The
// start itself is still processed normally so an extension run that begins
// right after a reserved command keeps its attribution.
func (c *Coordinator) endSupersede(e Event) bool {
	if e.Type != eventAgentStart {
		return false
	}
	if c.op != nil {
		c.state = stateRunning
	} else {
		c.state = stateIdle
	}
	return true
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
	case c.op != nil && c.shutdownRequested:
		// The shutdown sequence is aborting this run; the restart notice is
		// its outcome, so no final-text query runs against a dying child.
		c.op = nil
		c.state = stateIdle
		c.stopHeartbeat()
		c.log.Info("operation settled during shutdown")
	case c.op != nil && c.reserved:
		// The run settled while a reserved command sequence was in flight: the
		// reserved report becomes the single terminal outcome, so this one is
		// held for the outcome command to discard or flush.
		c.deferred = c.op
		c.op = nil
		c.state = stateSuperseded
		c.stopHeartbeat()
		c.log.Info("operation settled during a reserved command; outcome deferred")
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
// Fire-and-forget requests are logged as metadata. Only the method is logged;
// a dialog can carry sensitive content that must never reach the log.
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
