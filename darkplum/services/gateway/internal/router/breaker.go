package router

import (
	"log"
	"sync"
	"time"
)

// breaker is the consecutive-failure circuit breaker used by
// SemanticRouterClassifier: after threshold consecutive call failures, it
// stays open for openDuration (fallback:circuit_open, no call attempted)
// before allowing a call through again. Safe for concurrent use.
type breaker struct {
	name         string // for log lines, e.g. "semantic router"
	threshold    int
	openDuration time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// newBreaker constructs a breaker. threshold and openDuration are
// CLASSIFIER_BREAKER_FAILURES / CLASSIFIER_BREAKER_OPEN_SECONDS (validated
// >= 1 / >= 5 at config load).
func newBreaker(name string, threshold int, openDuration time.Duration) *breaker {
	return &breaker{name: name, threshold: threshold, openDuration: openDuration}
}

// open reports whether the breaker is currently open. If the open window has
// elapsed since the last check, it closes the breaker (resetting the failure
// count) and logs once before returning false — so a caller only ever needs
// this one check-and-maybe-close call per attempt, with no separate
// background timer.
func (b *breaker) open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.openUntil.IsZero() {
		return false
	}
	if time.Now().Before(b.openUntil) {
		return true
	}

	b.openUntil = time.Time{}
	b.failures = 0
	log.Printf("router: %s circuit breaker closed — resuming normal service", b.name)
	return false
}

// recordOutcome updates the consecutive-failure count for one completed call
// attempt (callErr nil on success) and opens the breaker, logging once, the
// moment the count reaches threshold.
func (b *breaker) recordOutcome(callErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if callErr == nil {
		b.failures = 0
		return
	}

	b.failures++
	if b.failures >= b.threshold && b.openUntil.IsZero() {
		b.openUntil = time.Now().Add(b.openDuration)
		log.Printf("router: %s circuit breaker OPEN after %d consecutive failures — falling back to default_route for %s",
			b.name, b.failures, b.openDuration)
	}
}

// status reports the breaker's current state for /health. Unlike open, it
// never closes the breaker as a side effect — it reports what IS true right
// now, matching the policy.PgStore.Status / routing.Manager.Status pattern
// used elsewhere for diagnostic (non-enforcing) reads.
func (b *breaker) status() (openNow bool, consecutiveFailures int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.openUntil.IsZero() && time.Now().Before(b.openUntil), b.failures
}
