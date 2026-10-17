// Package credsource reads a file-backed vendor API key from a Kubernetes
// secret volume mount, shared by every vendor provider package (anthropic
// today; openai next) so credential-rotation handling is written once.
//
// A projected secret volume mounts each key as a chain of symlinks
// (mountdir/<file> -> mountdir/..data/<file> -> mountdir/..2024_.../<file>),
// and kubelet swaps the ..data symlink atomically on rotation. Reading
// through the top-level path (os.ReadFile) always follows the symlink chain
// to whatever ..data currently points at, so content — never the symlink's
// own mtime, which does not change across a ..data swap — is what Source
// compares to detect a rotation.
package credsource

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotConfigured is returned by (*Source).Key when the key file does not
// exist at all. This is the expected, quiet state for a vendor the operator
// has not set up yet — callers should treat it as "not configured", not as a
// degraded/error condition worth logging loudly.
var ErrNotConfigured = errors.New("credsource: key file not present")

// defaultRecheckInterval bounds how often Key() actually touches the
// filesystem. Key() is cheap enough to call on every request; between
// rechecks it returns the cached result.
const defaultRecheckInterval = 3 * time.Second

// Source reads and caches a single vendor API key file. Safe for concurrent
// use. The zero value is not usable; construct with New.
type Source struct {
	path            string
	recheckInterval time.Duration

	mu        sync.Mutex
	lastCheck time.Time
	key       string
	version   uint64
	err       error

	// tempRevertAt is non-zero while a temporary recheck interval (see
	// SetTemporaryRecheckInterval) is active, naming the exact deadline the
	// pending revert timer is scheduled against — used to detect a stale
	// timer firing after a LATER call already replaced it, so reverting
	// never clobbers a still-active extension.
	tempRevertAt time.Time
}

// New returns a Source reading path (e.g.
// "/var/run/darkplum/provider-keys/anthropic-api-key"). The file is not read
// until the first call to Key().
func New(path string) *Source {
	return &Source{path: path, recheckInterval: defaultRecheckInterval}
}

// Key returns the current key, a version that increments every time the key
// CONTENT changes (so callers can cache an expensive-to-build client keyed
// on version and only rebuild it when the value actually changes), and an
// error.
//
// The error is ErrNotConfigured (checkable with errors.Is) when the key file
// does not exist — the caller should treat the vendor as simply not
// configured. Any other non-nil error means the file exists but could not be
// read or is empty after trimming whitespace — a degraded, worth-logging
// condition distinct from "not configured". On error, the returned key is
// always "".
//
// Cheap enough to call once per request: the underlying file is re-read at
// most once per recheckInterval; calls within that window return the cached
// result. Concurrent calls are safe — internally serialised by a mutex, so a
// rotation landing mid-call is always observed as a clean before/after value,
// never a torn read.
func (s *Source) Key() (key string, version uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if !s.lastCheck.IsZero() && now.Sub(s.lastCheck) < s.recheckInterval {
		return s.key, s.version, s.err
	}
	s.lastCheck = now

	newKey, readErr := readKeyFile(s.path)
	if readErr != nil {
		s.key = ""
		s.err = readErr
		return "", s.version, readErr
	}

	if newKey != s.key {
		s.version++
		s.key = newKey
	}
	s.err = nil
	return s.key, s.version, nil
}

// SetTemporaryRecheckInterval shortens the normal recheckInterval to
// interval for duration, then automatically reverts to
// defaultRecheckInterval. Used by the gateway's POST
// /v1/providers/:vendor/refresh hint (see routing.Manager.RefreshSource and
// internal/handler.ProvidersHandler): a freshly installed or rotated key
// needs Key() to actually re-read the file on the very next tick rather than
// serving a cached miss/old value from just before the Secret landed.
//
// A second call while a temporary interval is already active EXTENDS the
// revert deadline rather than stacking a second pending revert timer: the
// stale timer from the first call detects (via tempRevertAt) that a later
// call has since replaced it, and does nothing.
func (s *Source) SetTemporaryRecheckInterval(interval, duration time.Duration) {
	s.mu.Lock()
	s.recheckInterval = interval
	revertAt := time.Now().Add(duration)
	s.tempRevertAt = revertAt
	s.mu.Unlock()

	time.AfterFunc(duration, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.tempRevertAt.Equal(revertAt) {
			// A later call already extended or replaced the pending revert;
			// this stale timer must not clobber it.
			return
		}
		s.recheckInterval = defaultRecheckInterval
		s.tempRevertAt = time.Time{}
	})
}

// readKeyFile reads path, trims surrounding whitespace, and distinguishes a
// missing file (ErrNotConfigured) from any other failure (permission denied,
// a directory, empty content, ...). The file content itself is never
// included in the returned error.
func readKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotConfigured
		}
		return "", fmt.Errorf("credsource: reading %s: %w", path, err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", fmt.Errorf("credsource: %s is empty", path)
	}
	return trimmed, nil
}

// String never includes the cached key — see the package doc's redaction
// requirement. Defining String (and GoString below) also prevents fmt's
// default %v/%+v struct reflection from printing Source's unexported key
// field directly.
func (s *Source) String() string {
	return fmt.Sprintf("credsource.Source(%s)", s.path)
}

// GoString mirrors String for %#v formatting, for the same redaction reason.
func (s *Source) GoString() string {
	return s.String()
}
