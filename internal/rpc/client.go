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

// Client controls one pi rpc subprocess over strict JSONL on stdin/stdout.
// A read goroutine always drains stdout so pi is never stalled, routes
// responses to pending sends by request ID, and delivers session and
// extension UI records on buffered channels.
type Client struct {
	log    *slog.Logger
	stdin  io.WriteCloser
	stdout io.Reader
	kill   func() error
	events chan Event
	ui     chan UIRequest

	writeMu   sync.Mutex
	stdinOpen bool

	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan clientResult

	dieOnce sync.Once
	died    chan struct{}
	termErr error
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
	go func() { c.finish(cmd.Wait()) }()
	return c, nil
}

// newClient wires a client over arbitrary streams without spawning a process;
// the process tests use it with a helper child.
func newClient(stdin io.WriteCloser, stdout io.Reader, log *slog.Logger, kill func() error) *Client {
	return &Client{
		log:       log,
		stdin:     stdin,
		stdout:    stdout,
		kill:      kill,
		events:    make(chan Event, eventBuffer),
		ui:        make(chan UIRequest, uiBuffer),
		pending:   make(map[string]chan clientResult),
		died:      make(chan struct{}),
		stdinOpen: true,
	}
}

// Events returns the session event stream. One consumer (the runtime
// coordinator) must drain it; a full channel is a protocol integrity failure.
func (c *Client) Events() <-chan Event { return c.events }

// UIRequests returns the extension UI request stream. One consumer (the runtime
// coordinator) must drain it; a full channel is a protocol integrity failure.
func (c *Client) UIRequests() <-chan UIRequest { return c.ui }

// Send sends one command and waits for its correlated response or ctx
// cancellation. The command's ID, when empty, is assigned by the client.
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
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// AnswerUIDialog answers an extension dialog request by cancelling it.
func (c *Client) AnswerUIDialog(_ context.Context, id string) error {
	return c.write(CancelledUIResponse(id))
}

// CloseStdin requests an orderly pi shutdown. It is idempotent.
func (c *Client) CloseStdin() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.stdinOpen {
		return nil
	}
	c.stdinOpen = false
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
	if !c.stdinOpen {
		return errStdinClosed
	}
	return WriteRecord(c.stdin, v)
}

// readLoop drains stdout continuously and routes records. It runs until the
// stream ends or a protocol integrity failure occurs; it never drops a record
// silently.
func (c *Client) readLoop() {
	sc := NewScanner(c.stdout)
	for {
		rec, err := sc.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			c.fatal(fmt.Errorf("read pi stdout: %w", err))
			return
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := DecodeRecord(rec, &head); err != nil {
			c.fatal(err)
			return
		}
		switch head.Type {
		case "response":
			var r Response
			if err := DecodeRecord(rec, &r); err != nil {
				c.fatal(err)
				return
			}
			c.dispatchResponse(r)
		case UIRequestType:
			var u UIRequest
			if err := DecodeRecord(rec, &u); err != nil {
				c.fatal(err)
				return
			}
			c.deliverUI(u)
		case UIResponseType:
			// Pi never answers a request we did not send; an unexpected
			// extension_ui_response is noise, not a protocol violation.
			c.log.Warn("unexpected extension_ui_response from pi")
		default:
			var e Event
			if err := DecodeRecord(rec, &e); err != nil {
				c.fatal(err)
				return
			}
			c.deliverEvent(e)
		}
	}
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
		fail := err
		if fail == nil {
			fail = ErrProcessExited
		}
		c.failPending(fail)
		close(c.died)
	})
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
