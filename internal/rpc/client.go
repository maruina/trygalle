package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Channel capacities for the event and extension UI streams. A full channel is
// a protocol integrity failure (the coordinator stopped consuming), never a
// silent drop.
const (
	eventBuffer = 1024
	uiBuffer    = 1024
)

// ErrProcessExited reports that the pi process exited while a command response
// was still pending.
var ErrProcessExited = errors.New("pi process exited before responding")

var errStdinClosed = errors.New("pi stdin closed")

// clientResult is one completed send: either a response or a terminal failure.
type clientResult struct {
	resp Response
	err  error
}

// Activity is the most recent session event the client observed. It carries
// metadata only, never record content. The zero value means no event yet.
type Activity struct {
	Type string    // event type, e.g. "message_update"
	At   time.Time // when the client read the event
}

// Client controls one pi rpc subprocess over strict JSONL on stdin/stdout.
// A read goroutine always drains stdout so pi is never stalled, routes
// responses to pending sends by request ID, delivers state events and
// extension UI records on buffered channels, and records every session event
// as the last activity.
type Client struct {
	log    *slog.Logger
	stdin  io.WriteCloser
	stdout io.Reader
	kill   func() error
	events chan Event
	ui     chan UIRequest

	// writeMu serializes records on stdin. CloseStdin does not take it, so
	// closing stdin releases a write blocked on a child that stopped reading.
	writeMu     sync.Mutex
	stdinClosed atomic.Bool

	// readDone closes when readLoop returns. os/exec closes stdout in Wait, so
	// Wait must not start before readLoop has drained it.
	readDone chan struct{}

	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan clientResult

	dieOnce sync.Once
	died    chan struct{}
	termErr error

	activityMu sync.Mutex
	activity   Activity
}

// Option configures a Client.
type Option func(*options)

type options struct {
	log    *slog.Logger
	stderr io.Writer
}

// WithLogger sets the logger for client diagnostics. The default is
// slog.Default().
func WithLogger(log *slog.Logger) Option { return func(o *options) { o.log = log } }

// WithStderr directs the child process stderr to w instead of os.Stderr.
// stderr carries diagnostics only; it is never parsed as protocol data.
func WithStderr(w io.Writer) Option { return func(o *options) { o.stderr = w } }

// New spawns pi with the given arguments and returns a ready client. The
// event and extension UI subscriptions are established before any Send can run.
func New(bin string, args []string, opts ...Option) (*Client, error) {
	o := options{log: slog.Default(), stderr: os.Stderr}
	for _, opt := range opts {
		opt(&o)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stderr = o.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi: %w", err)
	}
	c := newClient(stdin, stdout, o.log, cmd.Process.Kill)
	go c.readLoop()
	go func() {
		<-c.readDone
		c.finish(cmd.Wait())
	}()
	return c, nil
}

// newClient wires a client over arbitrary streams without spawning a process;
// the process tests use it with a helper child.
func newClient(stdin io.WriteCloser, stdout io.Reader, log *slog.Logger, kill func() error) *Client {
	return &Client{
		log:      log,
		stdin:    stdin,
		stdout:   stdout,
		kill:     kill,
		events:   make(chan Event, eventBuffer),
		ui:       make(chan UIRequest, uiBuffer),
		pending:  make(map[string]chan clientResult),
		readDone: make(chan struct{}),
		died:     make(chan struct{}),
	}
}

// Events returns the state event stream (see isStateEvent). One consumer (the
// runtime coordinator) must drain it; a full channel is a protocol integrity
// failure.
func (c *Client) Events() <-chan Event { return c.events }

// UIRequests returns the extension UI request stream. One consumer (the runtime
// coordinator) must drain it; a full channel is a protocol integrity failure.
func (c *Client) UIRequests() <-chan UIRequest { return c.ui }

// LastActivity returns the type and arrival time of the most recent session
// event of any type, including activity-only events that Events does not
// deliver.
func (c *Client) LastActivity() Activity {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	return c.activity
}

// Send sends one command and waits for its correlated response, the client's
// terminal failure, or ctx cancellation. Send always assigns the request ID.
func (c *Client) Send(ctx context.Context, cmd Command) (Response, error) {
	cmd.ID = c.claimID()
	ch := make(chan clientResult, 1)
	c.mu.Lock()
	c.pending[cmd.ID] = ch
	c.mu.Unlock()
	defer c.forget(cmd.ID)

	if err := c.write(cmd); err != nil {
		return Response{}, err
	}
	return c.awaitResponse(ctx, ch)
}

func (c *Client) awaitResponse(ctx context.Context, ch <-chan clientResult) (Response, error) {
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-c.died:
		return c.responseOrTerminal(ch)
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// responseOrTerminal gives a queued response precedence over process death.
func (c *Client) responseOrTerminal(ch <-chan clientResult) (Response, error) {
	select {
	case r := <-ch:
		return r.resp, r.err
	default:
		return Response{}, pendingErr(c.Wait())
	}
}

// AnswerUIDialog answers an extension dialog request by cancelling it.
func (c *Client) AnswerUIDialog(_ context.Context, id string) error {
	return c.write(CancelledUIResponse(id))
}

// CloseStdin requests an orderly pi shutdown. It is idempotent.
func (c *Client) CloseStdin() error {
	if c.stdinClosed.Swap(true) {
		return nil
	}
	return c.stdin.Close()
}

// Kill terminates a child that missed its bounded graceful-exit deadline. It is
// idempotent with respect to an already-exited child.
func (c *Client) Kill() error {
	if c.kill == nil {
		return nil
	}
	if err := c.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Wait returns the terminal condition of the client: nil for an orderly exit
// after stdin close, or the process failure or protocol integrity failure that
// made the session unusable. All callers observe the same cached result.
func (c *Client) Wait() error {
	<-c.died
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termErr
}

func (c *Client) claimID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return "req-" + strconv.FormatUint(c.nextID, 10)
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

func (c *Client) write(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdinClosed.Load() {
		return errStdinClosed
	}
	return WriteRecord(c.stdin, v)
}

// readLoop drains stdout continuously and routes records. It runs until the
// stream ends or a protocol integrity failure occurs; it never drops a record
// silently.
func (c *Client) readLoop() {
	defer close(c.readDone)
	scanner := NewScanner(c.stdout)
	for {
		err := c.readNextRecord(scanner)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.fatal(err)
			return
		}
	}
}

func (c *Client) readNextRecord(scanner *Scanner) error {
	rec, err := scanner.Next()
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("read pi stdout: %w", err)
	}
	if err != nil {
		return err
	}
	return c.dispatchRecord(rec)
}

func (c *Client) dispatchRecord(rec []byte) error {
	typeName, err := recordType(rec)
	if err != nil {
		return err
	}
	switch typeName {
	case "response":
		err = c.dispatchResponseRecord(rec)
	case UIRequestType:
		err = c.dispatchUIRequestRecord(rec)
	case UIResponseType:
		// Pi never answers a request we did not send; an unexpected
		// extension_ui_response is noise, not a protocol violation.
		c.log.Warn("unexpected extension_ui_response from pi")
	default:
		err = c.dispatchEventRecord(rec, typeName)
	}
	return err
}

func recordType(rec []byte) (string, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := DecodeRecord(rec, &head); err != nil {
		return "", err
	}
	return head.Type, nil
}

func (c *Client) dispatchResponseRecord(rec []byte) error {
	var response Response
	if err := DecodeRecord(rec, &response); err != nil {
		return err
	}
	c.dispatchResponse(response)
	return nil
}

func (c *Client) dispatchUIRequestRecord(rec []byte) error {
	var request UIRequest
	if err := DecodeRecord(rec, &request); err != nil {
		return err
	}
	c.deliverUI(request)
	return nil
}

func (c *Client) dispatchEventRecord(rec []byte, eventType string) error {
	c.recordActivity(eventType)
	if !isStateEvent(eventType) {
		return nil
	}
	var event Event
	if err := DecodeRecord(rec, &event); err != nil {
		return err
	}
	c.deliverEvent(event)
	return nil
}

func (c *Client) dispatchResponse(r Response) {
	if r.ID == "" {
		// Pi rejects a malformed command with a parse response without an id.
		// deliberate: the error string may echo the rejected command (a
		// Trygalle bug, never user content we choose to send); the payload
		// redaction policy (follow-up #5) owns truncation.
		c.log.Warn("pi rejected a malformed command", "command", r.Command, "error", r.Error)
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[r.ID]
	c.mu.Unlock()
	if !ok {
		c.log.Warn("response for unknown request", "id", r.ID, "command", r.Command)
		return
	}
	select {
	case ch <- clientResult{resp: r}:
	default:
		// The waiter left without consuming (deadline); the response has no
		// further owner.
		c.log.Warn("response arrived after its request completed", "id", r.ID)
	}
}

func (c *Client) recordActivity(eventType string) {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	c.activity = Activity{Type: eventType, At: time.Now()}
}

func (c *Client) deliverEvent(e Event) {
	select {
	case c.events <- e:
	default:
		c.fatal(&ProtocolError{Err: errors.New("session event channel full: coordinator not consuming")})
	}
}

func (c *Client) deliverUI(u UIRequest) {
	select {
	case c.ui <- u:
	default:
		c.fatal(&ProtocolError{Err: errors.New("extension UI channel full: coordinator not consuming")})
	}
}

// fatal handles a protocol integrity failure: it logs metadata (never record
// content), fails pending sends, terminates the child, and publishes the
// failure as the terminal result so the runtime exits.
func (c *Client) fatal(err error) {
	c.log.Error("pi rpc protocol integrity failure", "error", err)
	c.finish(err)
	_ = c.Kill()
}

// finish publishes the first terminal condition of the client. It runs exactly
// once: either the child's exit result or a protocol integrity failure.
func (c *Client) finish(err error) {
	c.dieOnce.Do(func() {
		c.mu.Lock()
		c.termErr = err
		c.mu.Unlock()
		c.failPending(pendingErr(err))
		close(c.died)
	})
}

// pendingErr is the error a send observes when the client died before it got
// a response.
func pendingErr(termErr error) error {
	if termErr == nil {
		return ErrProcessExited
	}
	return termErr
}

// failPending fails every outstanding send. A response delivered by the read
// goroutine wins over the failure when it was already queued.
func (c *Client) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.pending {
		select {
		case ch <- clientResult{err: err}:
		default:
		}
	}
}
