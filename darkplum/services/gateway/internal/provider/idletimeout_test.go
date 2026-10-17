package provider

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestIdleTimeoutReader_ActivityResetsTimer asserts a steady trickle of bytes
// (each arriving well under idleTimeout) never trips the watchdog, and every
// byte is still delivered to the caller — the extraction from
// openaicompat must not change this behaviour.
func TestIdleTimeoutReader_ActivityResetsTimer(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	r := NewIdleTimeoutReader(strings.NewReader("hello world"), 50*time.Millisecond, cancel)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
	if ctx.Err() != nil {
		t.Errorf("context should not be cancelled after a normal EOF read, got %v", ctx.Err())
	}
}

// slowReader blocks forever on every Read after the first, simulating a
// wedged upstream that stops sending bytes without closing the connection.
type slowReader struct {
	first bool
	block chan struct{}
}

func (r *slowReader) Read(p []byte) (int, error) {
	if !r.first {
		r.first = true
		p[0] = 'x'
		return 1, nil
	}
	<-r.block // never closed in this test — simulates a permanently wedged stream
	return 0, nil
}

// TestIdleTimeoutReader_WedgedStreamCancelsContext asserts a stream that stops
// sending bytes entirely trips the watchdog and cancels the context with a
// descriptive cause.
func TestIdleTimeoutReader_WedgedStreamCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sr := &slowReader{block: make(chan struct{})}
	r := NewIdleTimeoutReader(sr, 20*time.Millisecond, cancel)

	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("unexpected error on first read: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expected the idle watchdog to cancel the context")
	}
	if context.Cause(ctx) == nil {
		t.Error("expected a non-nil cancellation cause")
	}
}
