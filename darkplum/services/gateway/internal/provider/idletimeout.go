package provider

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// IdleTimeoutReader wraps an io.Reader and cancels a context if no bytes are
// received within the configured idle window. The timer is reset on every
// successful Read call that returns at least one byte, so a slow but active
// stream (e.g. long generation) is never killed. Only a stream that wedges
// completely (no bytes for idleTimeout) triggers the cancellation.
//
// The cancel function is called with an explanatory error as its cause so that
// callers can distinguish idle-timeout errors from upstream errors via
// context.Cause.
//
// Originally implemented inside internal/provider/openaicompat as an
// unexported helper; extracted here, behaviour-preserving, so every provider
// that streams from an upstream connection (openaicompat today; future
// anthropic/openai HTTP streams) can share one idle watchdog instead of each
// reimplementing it.
type IdleTimeoutReader struct {
	r           io.Reader
	idleTimeout time.Duration
	cancelCause context.CancelCauseFunc

	mu    sync.Mutex
	timer *time.Timer
}

// NewIdleTimeoutReader wraps r with an idle-timeout watchdog. If no bytes
// arrive within idleTimeout, cancelCause is called with a descriptive error.
// The caller is responsible for calling the returned CancelCauseFunc when done
// (to stop the internal timer and avoid a goroutine leak).
func NewIdleTimeoutReader(r io.Reader, idleTimeout time.Duration, cancelCause context.CancelCauseFunc) *IdleTimeoutReader {
	itr := &IdleTimeoutReader{
		r:           r,
		idleTimeout: idleTimeout,
		cancelCause: cancelCause,
	}
	itr.timer = time.AfterFunc(idleTimeout, func() {
		cancelCause(fmt.Errorf("no bytes received from upstream for %s", idleTimeout))
	})
	return itr
}

// Read implements io.Reader. Each call that returns at least one byte resets
// the idle timer. If the underlying reader returns io.EOF or an error the
// timer is stopped to avoid a spurious cancellation after the stream ends.
func (r *IdleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > 0 {
		// Activity — reset the watchdog.
		r.timer.Reset(r.idleTimeout)
	}
	if err != nil {
		// Stream ended or errored — stop the timer so the cancelCause
		// is not called after the caller has already handled the EOF/error.
		r.timer.Stop()
	}
	return n, err
}
