package rpc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCancellationInterruptsStalledWrite(t *testing.T) {
	stdin := &stalledStdin{entered: make(chan struct{}), closed: make(chan struct{})}
	c := newClient(stdin, nil, discardLogger(), func() error { return nil })
	t.Cleanup(func() { _ = c.CloseStdin() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.Send(ctx, GetStateCommand())
		result <- err
	}()
	<-stdin.entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt write")
	}
	if err := c.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestCancellationWhileWaitingForWriterPreservesTransport(t *testing.T) {
	s := newScripted(t, discardLogger())
	c := s.client
	c.writeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.AnswerUIDialog(ctx, "dialog")
	<-c.writeGate
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AnswerUIDialog = %v", err)
	}
	select {
	case <-c.died:
		t.Fatal("waiting cancellation terminated transport")
	case record := <-s.wrote:
		t.Fatalf("cancelled writer sent %s", record)
	default:
	}
}
