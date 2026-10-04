package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lineSource serves scripted stdout records on demand and returns io.EOF after
// close. Records are delivered one per Read; the scanner accumulates chunks.
type lineSource struct {
	lines  chan []byte
	closed chan struct{}
}

func newLineSource() *lineSource {
	return &lineSource{lines: make(chan []byte), closed: make(chan struct{})}
}

// emit queues one stdout record line for the client's read loop.
func (l *lineSource) emit(line string) {
	l.lines <- []byte(line + "\n")
}

// close ends the stream; pending and later emits block forever, so tests must
// finish their script before cleanup.
func (l *lineSource) close() { close(l.closed) }

func (l *lineSource) Read(p []byte) (int, error) {
	select {
	case line := <-l.lines:
		n := copy(p, line)
		if n != len(line) {
			panic("scripted record larger than read buffer") // records are tiny
		}
		return n, nil
	case <-l.closed:
		return 0, io.EOF
	}
}

// scripted wires a client over in-memory streams with a kill counter and the
// records the client writes to stdin.
type scripted struct {
	t          *testing.T
	client     *Client
	stdinWrite *io.PipeWriter
	stdout     *lineSource
	killCalls  int
	killMu     sync.Mutex
	wrote      chan []byte
}

func newScripted(t *testing.T, log *slog.Logger) *scripted {
	t.Helper()
	src := newLineSource()
	stdinRead, stdinWrite := io.Pipe()
	s := &scripted{
		t:          t,
		stdinWrite: stdinWrite,
		stdout:     src,
		wrote:      make(chan []byte, 64),
	}
	c := newClient(stdinWrite, src, log, func() error {
		s.killMu.Lock()
		s.killCalls++
		s.killMu.Unlock()
		return nil
	})
	s.client = c
	go c.readLoop()
	go func() {
		sc := NewScanner(stdinRead)
		for {
			rec, err := sc.Next()
			if err != nil {
				return
			}
			s.wrote <- append([]byte(nil), rec...)
		}
	}()
	t.Cleanup(func() {
		src.close()
		_ = stdinWrite.Close()
		_ = stdinRead.Close()
	})
	return s
}

func (s *scripted) kills() int {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	return s.killCalls
}

// emit writes one stdout record for the client's read loop.
func (s *scripted) emit(line string) {
	s.stdout.emit(line)
}

// respond answers the command the client last wrote on stdin.
func (s *scripted) respond(cmd Command, data string) {
	s.t.Helper()
	s.emit(`{"id":` + jsonQuote(cmd.ID) + `,"type":"response","command":` + jsonQuote(cmd.Type) + `,"success":true,"data":` + data + `}`)
}

// wroteCommand returns the command the client wrote on stdin.
func (s *scripted) wroteCommand() Command {
	s.t.Helper()
	rec := <-s.wrote
	var cmd Command
	if err := DecodeRecord(rec, &cmd); err != nil {
		s.t.Fatalf("decode written command: %v", err)
	}
	return cmd
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSendCorrelatesResponse proves request/response correlation by request ID.
func TestSendCorrelatesResponse(t *testing.T) {
	s := newScripted(t, discardLogger())
	go func() {
		cmd := s.wroteCommand()
		s.respond(cmd, `{"disposition":"started"}`)
	}()
	resp, err := s.client.Send(context.Background(), PromptCommand("Hello", "steer"))
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if !resp.Success || resp.Command != "prompt" {
		t.Errorf("response = %+v", resp)
	}
	var d PromptData
	if err := resp.Decode(&d); err != nil {
		t.Fatalf("decode disposition: %v", err)
	}
	if d.Disposition != DispositionStarted {
		t.Errorf("disposition = %q, want %q", d.Disposition, DispositionStarted)
	}
}

// TestDeadlineExpiry proves a caller-supplied deadline bounds each command.
func TestDeadlineExpiry(t *testing.T) {
	s := newScripted(t, discardLogger())
	go func() { s.wroteCommand() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.client.Send(ctx, GetStateCommand())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want context deadline exceeded", err)
	}
	// The timed-out request must not poison later sends.
	go func() {
		cmd := s.wroteCommand()
		s.respond(cmd, `{"disposition":"started"}`)
	}()
	if _, err := s.client.Send(context.Background(), PromptCommand("after", "steer")); err != nil {
		t.Fatalf("Send after deadline error: %v", err)
	}
}

// TestFastCompletionBeforeResponse proves events emitted before a command
// response are not lost (R2 subscription-ordering scenario).
func TestFastCompletionBeforeResponse(t *testing.T) {
	s := newScripted(t, discardLogger())
	go func() {
		cmd := s.wroteCommand()
		s.emit(`{"type":"agent_settled"}`)
		s.respond(cmd, `{"disposition":"started"}`)
	}()
	if _, err := s.client.Send(context.Background(), PromptCommand("hi", "steer")); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	select {
	case e := <-s.client.Events():
		if e.Type != EventAgentSettled {
			t.Errorf("event = %q, want %q", e.Type, EventAgentSettled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast completion event was lost")
	}
}

// TestGarbageRecordIsProtocolError proves a non-conforming stdout record fails
// the in-flight send with a typed error and terminates the client.
func TestGarbageRecordIsProtocolError(t *testing.T) {
	s := newScripted(t, discardLogger())
	go func() {
		s.wroteCommand()
		s.emit("not json")
	}()
	_, err := s.client.Send(context.Background(), GetStateCommand())
	var perr *ProtocolError
	if !errors.As(err, &perr) {
		t.Fatalf("Send error = %v, want *ProtocolError", err)
	}
	if s.kills() == 0 {
		t.Error("expected the child to be killed after a protocol integrity failure")
	}
	waitErr := s.client.Wait()
	if !errors.As(waitErr, &perr) {
		t.Errorf("Wait error = %v, want *ProtocolError", waitErr)
	}
}

// TestParseResponseLoggedNotFatal proves a pi parse error (no request id) is
// logged and the client keeps running.
func TestParseResponseLoggedNotFatal(t *testing.T) {
	var logBuf mutexBuffer
	s := newScripted(t, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.emit(`{"type":"response","command":"parse","success":false,"error":"Failed to parse command: x"}`)
	go func() {
		cmd := s.wroteCommand()
		s.respond(cmd, `{"disposition":"started"}`)
	}()
	resp, err := s.client.Send(context.Background(), PromptCommand("hi", "steer"))
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if !resp.Success {
		t.Errorf("response = %+v, want success after a parse response", resp)
	}
	if !strings.Contains(logBuf.String(), "pi rejected a malformed command") {
		t.Errorf("parse response not logged; log = %q", logBuf.String())
	}
}

// TestAnswerUIDialog proves extension UI dialogs are answered on stdin.
func TestAnswerUIDialog(t *testing.T) {
	s := newScripted(t, discardLogger())
	s.emit(`{"type":"extension_ui_request","id":"uuid-1","method":"select","title":"T","options":["A"],"timeout":10000}`)
	select {
	case req := <-s.client.UIRequests():
		if req.Method != "select" || req.ID != "uuid-1" {
			t.Errorf("UI request = %+v", req)
		}
		if err := s.client.AnswerUIDialog(context.Background(), req.ID); err != nil {
			t.Fatalf("AnswerUIDialog error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI request was not delivered")
	}
	rec := <-s.wrote
	var u UIResponse
	if err := DecodeRecord(rec, &u); err != nil {
		t.Fatalf("decode written UI response: %v", err)
	}
	if u.Type != UIResponseType || u.ID != "uuid-1" || !u.Cancelled {
		t.Errorf("UI response = %+v, want cancel answer", u)
	}
}

// TestFullEventChannelIsProtocolError proves a full event channel is a protocol
// integrity failure, never a silent drop.
func TestFullEventChannelIsProtocolError(t *testing.T) {
	s := newScripted(t, discardLogger())
	go func() {
		// The read loop consumes each record; the last one overflows the buffer.
		for i := 0; i <= eventBuffer; i++ {
			s.emit(`{"type":"agent_start"}`)
		}
	}()
	waitErr := s.client.Wait()
	if _, ok := errors.AsType[*ProtocolError](waitErr); !ok {
		t.Fatalf("Wait error = %v, want *ProtocolError", waitErr)
	}
}

// TestActivityOnlyEventsNotDelivered proves per-token and unconsumed event
// types update LastActivity without filling the event channel, so a token
// stream longer than the buffer is not a protocol integrity failure.
func TestActivityOnlyEventsNotDelivered(t *testing.T) {
	s := newScripted(t, discardLogger())
	if a := s.client.LastActivity(); a != (Activity{}) {
		t.Fatalf("LastActivity before any event = %+v, want zero", a)
	}
	start := time.Now()
	go func() {
		s.emit(`{"type":"message_start","message":{"role":"assistant"}}`)
		for range eventBuffer + 10 {
			s.emit(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"x"}}`)
		}
		s.emit(`{"type":"tool_execution_update","toolCallId":"t1"}`)
		// A UI request marks the point where the read loop has processed
		// every earlier record.
		s.emit(`{"type":"extension_ui_request","id":"sync","method":"notify"}`)
	}()
	select {
	case <-s.client.UIRequests():
	case <-time.After(5 * time.Second):
		t.Fatal("read loop did not reach the sync record")
	}
	if n := len(s.client.Events()); n != 0 {
		t.Errorf("events delivered = %d, want 0 activity-only events delivered", n)
	}
	a := s.client.LastActivity()
	if a.Type != "tool_execution_update" || a.At.Before(start) {
		t.Errorf("LastActivity = %+v, want tool_execution_update after %v", a, start)
	}

	s.emit(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"boom"}}`)
	select {
	case e := <-s.client.Events():
		if e.Type != EventMessageEnd || e.Message == nil || e.Message.StopReason != "error" {
			t.Errorf("event = %+v, want message_end with the failure fields", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message_end was not delivered")
	}
	if a := s.client.LastActivity(); a.Type != EventMessageEnd {
		t.Errorf("LastActivity type = %q, want %q", a.Type, EventMessageEnd)
	}
}

// TestScriptedCloseStdin stops future writes and is idempotent.
func TestScriptedCloseStdin(t *testing.T) {
	s := newScripted(t, discardLogger())
	if err := s.client.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin error: %v", err)
	}
	if err := s.client.CloseStdin(); err != nil {
		t.Fatalf("second CloseStdin error (must be idempotent): %v", err)
	}
	_, err := s.client.Send(context.Background(), GetStateCommand())
	if !errors.Is(err, errStdinClosed) {
		t.Fatalf("Send after close = %v, want errStdinClosed", err)
	}
}

// TestSendAfterFatalFailsFast proves a send that starts after a protocol
// integrity failure returns the terminal error instead of waiting for its
// context.
func TestSendAfterFatalFailsFast(t *testing.T) {
	s := newScripted(t, discardLogger())
	s.emit("not json")
	_ = s.client.Wait()
	go func() { s.wroteCommand() }()
	sendErr := make(chan error, 1)
	go func() {
		_, err := s.client.Send(context.Background(), GetStateCommand())
		sendErr <- err
	}()
	select {
	case err := <-sendErr:
		if _, ok := errors.AsType[*ProtocolError](err); !ok {
			t.Fatalf("Send error = %v, want *ProtocolError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send after a fatal failure did not return")
	}
}

// stalledStdin is a child stdin that never drains: Write blocks until Close.
type stalledStdin struct {
	once    sync.Once
	entered chan struct{}
	closed  chan struct{}
	close   sync.Once
}

func (s *stalledStdin) Write([]byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *stalledStdin) Close() error {
	s.close.Do(func() { close(s.closed) })
	return nil
}

// TestCloseStdinReleasesStalledWrite proves CloseStdin does not wait behind a
// write to a child that stopped reading, so a bounded stop can proceed.
func TestCloseStdinReleasesStalledWrite(t *testing.T) {
	stdin := &stalledStdin{entered: make(chan struct{}), closed: make(chan struct{})}
	src := newLineSource()
	t.Cleanup(src.close)
	c := newClient(stdin, src, discardLogger(), func() error { return nil })
	go c.readLoop()

	sendErr := make(chan error, 1)
	go func() {
		_, err := c.Send(context.Background(), GetStateCommand())
		sendErr <- err
	}()
	<-stdin.entered

	closed := make(chan error, 1)
	go func() { closed <- c.CloseStdin() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseStdin error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStdin blocked behind a stalled write")
	}
	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("Send = nil error after stdin close, want a write failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled Send did not return after CloseStdin")
	}
}

// mutexBuffer is a goroutine-safe byte buffer for log capture assertions.
type mutexBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (m *mutexBuffer) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.Write(p)
}

func (m *mutexBuffer) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.String()
}
