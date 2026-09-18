package router

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBreaker_ConcurrentRecordOutcome_OpensOnceAndClosesAfterWindow drives a
// breaker's recordOutcome/open/status from many goroutines at once (run with
// -race), mirroring real usage where multiple in-flight requests hit the
// same *breaker concurrently. It asserts three things the mutex-protected
// implementation promises: the breaker opens exactly once when the
// consecutive-failure threshold is reached (never re-logged/re-armed by
// later concurrent failures while already open — see recordOutcome's
// b.openUntil.IsZero() guard), the run never deadlocks, and the breaker
// closes again once the open window has elapsed.
func TestBreaker_ConcurrentRecordOutcome_OpensOnceAndClosesAfterWindow(t *testing.T) {
	const threshold = 10
	const openWindow = 500 * time.Millisecond
	const numGoroutines = 50
	const opsPerGoroutine = 100

	br := newBreaker("concurrency test", threshold, openWindow)

	// recordOutcome/open log their state transitions via the standard "log"
	// package — capture it to count transitions instead of poking at
	// breaker's unexported fields from concurrently-running goroutines.
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	origOutput, origFlags := log.Writer(), log.Flags()
	log.SetOutput(lockedWriter{&logBuf, &logMu})
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	errFail := errors.New("synthetic failure")

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				// A mix of successes (~12.5%) and failures (~87.5%),
				// interleaved with the same open()/status() calls a real
				// caller makes around a dispatch attempt — high enough a
				// failure ratio that the shared consecutive-failure counter
				// is certain to cross threshold well before the open window
				// (500ms) elapses, without making the test flaky.
				if (id+j)%8 == 0 {
					br.recordOutcome(nil)
				} else {
					br.recordOutcome(errFail)
				}
				br.open()
				br.status()
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("breaker concurrency test did not finish — suspected deadlock")
	}

	openNow, failures := br.status()
	if !openNow {
		t.Fatalf("expected breaker to be open after %d concurrent goroutines recording mostly-failures (consecutive_failures=%d)", numGoroutines, failures)
	}

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()

	if openCount := strings.Count(logged, "circuit breaker OPEN"); openCount != 1 {
		t.Errorf("expected exactly one circuit breaker OPEN transition despite thousands of concurrent failures past threshold, got %d:\n%s", openCount, logged)
	}

	time.Sleep(openWindow + 150*time.Millisecond)

	// status() computes openness live from time.Now() vs. the stored
	// deadline and never mutates state — so it must already report closed
	// once the window has elapsed, before open() ever runs again.
	if openNow, _ := br.status(); openNow {
		t.Error("expected status() to report closed once the open window has elapsed")
	}

	if stillOpen := br.open(); stillOpen {
		t.Error("expected open() to report closed once the open window has elapsed")
	}

	logMu.Lock()
	logged = logBuf.String()
	logMu.Unlock()

	if closeCount := strings.Count(logged, "circuit breaker closed"); closeCount != 1 {
		t.Errorf("expected exactly one circuit breaker closed transition, got %d:\n%s", closeCount, logged)
	}
}

// lockedWriter serialises writes to an underlying *bytes.Buffer — needed
// because the standard "log" package's own internal mutex only protects one
// Logger's formatting, not a shared io.Writer written to by both goroutines'
// breaker log lines and the test's later reads of buf.String().
type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
