package credsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKey_NotConfiguredWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "does-not-exist"))

	key, version, err := s.Key()
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
	if version != 0 {
		t.Errorf("expected version 0, got %d", version)
	}
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

func TestKey_UnreadableIsDistinctFromNotConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-key")
	if err := os.WriteFile(path, []byte("   \n\t"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	key, _, err := s.Key()
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
	if err == nil {
		t.Fatal("expected an error for an empty (whitespace-only) key file")
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Error("an empty file must not be classified as ErrNotConfigured")
	}
}

func TestKey_ReadsAndTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("  sk-ant-abc123  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	key, version, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-ant-abc123" {
		t.Errorf("expected trimmed key, got %q", key)
	}
	if version != 1 {
		t.Errorf("expected version 1 on first successful read, got %d", version)
	}
}

func TestKey_VersionUnchangedWhenContentUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-ant-same"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	s.recheckInterval = 0 // force a real re-read on every call

	_, v1, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, v2, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v1 != v2 {
		t.Errorf("expected version to stay stable across reads of unchanged content: v1=%d v2=%d", v1, v2)
	}
}

func TestKey_VersionBumpsOnRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-ant-old"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	s.recheckInterval = 0

	key1, v1, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key1 != "sk-ant-old" {
		t.Fatalf("unexpected key: %q", key1)
	}

	if err := os.WriteFile(path, []byte("sk-ant-new"), 0o644); err != nil {
		t.Fatal(err)
	}

	key2, v2, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key2 != "sk-ant-new" {
		t.Errorf("expected rotated key, got %q", key2)
	}
	if v2 <= v1 {
		t.Errorf("expected version to increase on rotation: v1=%d v2=%d", v1, v2)
	}
}

func TestKey_CachesWithinRecheckInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-ant-cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	s.recheckInterval = time.Hour // effectively "never recheck" for this test

	if _, _, err := s.Key(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Rewrite the file; the cached value must still be served since the
	// recheck interval has not elapsed.
	if err := os.WriteFile(path, []byte("sk-ant-rotated"), 0o644); err != nil {
		t.Fatal(err)
	}

	key, _, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-ant-cached" {
		t.Errorf("expected cached key to still be served within the recheck interval, got %q", key)
	}
}

// TestKey_SymlinkSwapRotation exercises the actual Kubernetes projected-secret
// shape: mountdir/key -> mountdir/..data/key -> mountdir/..2024_a/key, with
// ..data atomically re-pointed at a second timestamped directory on rotation
// (via a rename, matching kubelet's own atomic-swap behaviour).
func TestKey_SymlinkSwapRotation(t *testing.T) {
	dir := t.TempDir()

	gen1 := filepath.Join(dir, "..2024_01_01")
	gen2 := filepath.Join(dir, "..2024_01_02")
	if err := os.Mkdir(gen1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(gen2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen1, "anthropic-api-key"), []byte("sk-ant-gen1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen2, "anthropic-api-key"), []byte("sk-ant-gen2"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataLink := filepath.Join(dir, "..data")
	if err := os.Symlink(gen1, dataLink); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, "anthropic-api-key")
	if err := os.Symlink(filepath.Join("..data", "anthropic-api-key"), keyPath); err != nil {
		t.Fatal(err)
	}

	s := New(keyPath)
	s.recheckInterval = 0

	key, v1, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-ant-gen1" {
		t.Fatalf("expected gen1 key, got %q", key)
	}

	// Atomic swap: point ..data at gen2 via rename of a staged symlink, exactly
	// as kubelet does.
	staged := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(gen2, staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, dataLink); err != nil {
		t.Fatal(err)
	}

	key, v2, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-ant-gen2" {
		t.Errorf("expected gen2 key after rotation, got %q", key)
	}
	if v2 <= v1 {
		t.Errorf("expected version to bump across the symlink swap: v1=%d v2=%d", v1, v2)
	}
}

// TestKey_ConcurrentAccessDuringRotation exercises concurrent Key() calls
// while the underlying file is repeatedly rewritten (including through a
// ..data symlink swap), under go test -race. Every observed key must be one
// of the values actually written — never a torn or corrupted read.
func TestKey_ConcurrentAccessDuringRotation(t *testing.T) {
	dir := t.TempDir()

	gen1 := filepath.Join(dir, "..2024_01_01")
	gen2 := filepath.Join(dir, "..2024_01_02")
	if err := os.Mkdir(gen1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(gen2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen1, "key"), []byte("sk-ant-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen2, "key"), []byte("sk-ant-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataLink := filepath.Join(dir, "..data")
	if err := os.Symlink(gen1, dataLink); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key")
	if err := os.Symlink(filepath.Join("..data", "key"), keyPath); err != nil {
		t.Fatal(err)
	}

	s := New(keyPath)
	s.recheckInterval = 0

	valid := map[string]bool{"sk-ant-a": true, "sk-ant-b": true}

	var readers sync.WaitGroup
	stop := make(chan struct{})

	// Readers.
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				key, _, err := s.Key()
				if err == nil && !valid[key] {
					t.Errorf("observed unexpected key value %q", key)
				}
			}
		}()
	}

	// Writer: swaps ..data back and forth between gen1 and gen2, then signals
	// the readers to stop once it's done.
	targets := []string{gen2, gen1}
	for i := 0; i < 50; i++ {
		staged := filepath.Join(dir, fmt.Sprintf("..data_tmp_%d", i))
		if err := os.Symlink(targets[i%2], staged); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := os.Rename(staged, dataLink); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}
	close(stop)
	readers.Wait()
}

// ---------------------------------------------------------------------------
// SetTemporaryRecheckInterval
// ---------------------------------------------------------------------------

// TestSetTemporaryRecheckInterval_ShortensAndReverts proves the core refresh
// hint contract: a temporary interval is applied immediately, and Key()
// actually re-reads the file within it (unlike the normal, longer cache
// window — see TestKey_CachesWithinRecheckInterval), then automatically
// reverts to defaultRecheckInterval once the window elapses.
func TestSetTemporaryRecheckInterval_ShortensAndReverts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-before"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	s.recheckInterval = time.Hour // start from "effectively never recheck"

	if _, _, err := s.Key(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s.SetTemporaryRecheckInterval(5*time.Millisecond, 20*time.Millisecond)

	if err := os.WriteFile(path, []byte("sk-after"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force lastCheck far enough in the past that the shortened interval
	// alone decides whether a reread happens (matching every other test in
	// this file's approach to not depending on real elapsed wall-clock time
	// for the recheck-window assertion itself).
	s.mu.Lock()
	s.lastCheck = time.Now().Add(-10 * time.Millisecond)
	s.mu.Unlock()

	key, _, err := s.Key()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-after" {
		t.Errorf("expected the shortened recheck interval to pick up the rotation, got %q", key)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		reverted := s.recheckInterval == defaultRecheckInterval
		s.mu.Unlock()
		if reverted {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("expected recheckInterval to automatically revert to defaultRecheckInterval")
}

// TestSetTemporaryRecheckInterval_SecondCallExtendsRatherThanStacks asserts
// that a second call while a temporary interval is already active extends
// the deadline instead of leaving a stale timer that reverts too early.
func TestSetTemporaryRecheckInterval_SecondCallExtendsRatherThanStacks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(path)

	s.SetTemporaryRecheckInterval(time.Millisecond, 20*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	// Extend before the first window's revert timer would have fired.
	s.SetTemporaryRecheckInterval(time.Millisecond, 40*time.Millisecond)

	// The first timer fires at ~20ms; by 25ms the extension must still hold.
	time.Sleep(15 * time.Millisecond)
	s.mu.Lock()
	interval := s.recheckInterval
	s.mu.Unlock()
	if interval != time.Millisecond {
		t.Fatalf("expected the extended temporary interval to still be active at ~25ms, got %v", interval)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		reverted := s.recheckInterval == defaultRecheckInterval
		s.mu.Unlock()
		if reverted {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("expected recheckInterval to eventually revert once the extended window elapses")
}

func TestSource_RedactsKeyFromStringAndGoString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	sentinel := "sk-ant-super-secret-sentinel"
	if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	if _, _, err := s.Key(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	str := s.String()
	if strings.Contains(str, sentinel) {
		t.Errorf("String() leaked the key: %q", str)
	}

	goStr := s.GoString()
	if strings.Contains(goStr, sentinel) {
		t.Errorf("GoString() leaked the key: %q", goStr)
	}

	verbose := fmt.Sprintf("%+v", s)
	if strings.Contains(verbose, sentinel) {
		t.Errorf("%%+v leaked the key: %q", verbose)
	}
}
