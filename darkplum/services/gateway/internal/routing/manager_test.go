package routing

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a simple injectable clock for TTL/Status tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// staticSource builds a Source whose Discover returns a fixed result. Useful
// for tests that don't need per-call behaviour.
func staticSource(name, kind, class, providerKey string, ids []string) Source {
	return Source{
		Name:        name,
		Kind:        kind,
		Class:       class,
		ProviderKey: providerKey,
		Discover: func(_ context.Context) ([]string, error) {
			return ids, nil
		},
	}
}

func TestManager_CollisionSelfHostedBeatsCloudManaged(t *testing.T) {
	vllm := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"claude-3-5-sonnet"})
	brk := staticSource("bedrock", "bedrock", ClassCloudManaged, "bedrock", []string{"claude-3-5-sonnet"})

	// Register bedrock first to prove the rule is class-based, not just
	// "first source wins".
	m := NewManager([]Source{brk, vllm}, 0)
	m.Refresh(context.Background())

	route, ok := m.Snapshot().Lookup("claude-3-5-sonnet")
	if !ok {
		t.Fatal("expected route for claude-3-5-sonnet")
	}
	if route.ProviderKey != "vllm-default" {
		t.Errorf("expected self_hosted (vllm-default) to win over cloud_managed, got provider key %q", route.ProviderKey)
	}
	if route.Class != ClassSelfHosted {
		t.Errorf("expected Class self_hosted, got %q", route.Class)
	}
}

func TestManager_CollisionAmongSelfHostedFirstRegisteredWins(t *testing.T) {
	first := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})
	second := staticSource("vllm-extra1", "vllm", ClassSelfHosted, "vllm-extra1", []string{"llama-3-8b"})

	m := NewManager([]Source{first, second}, 0)
	m.Refresh(context.Background())

	route, ok := m.Snapshot().Lookup("llama-3-8b")
	if !ok {
		t.Fatal("expected route for llama-3-8b")
	}
	if route.ProviderKey != "vllm-default" {
		t.Errorf("expected first-registered source to win, got provider key %q", route.ProviderKey)
	}
}

func TestManager_SourceFailureKeepsLastGood(t *testing.T) {
	calls := 0
	src := Source{
		Name: "flaky", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "flaky",
		Discover: func(_ context.Context) ([]string, error) {
			calls++
			if calls == 1 {
				return []string{"model-a", "model-b"}, nil
			}
			return nil, errors.New("upstream unreachable")
		},
	}

	m := NewManager([]Source{src}, 0)
	m.Refresh(context.Background()) // succeeds
	m.Refresh(context.Background()) // fails — must keep model-a/model-b

	table := m.Snapshot()
	if _, ok := table.Lookup("model-a"); !ok {
		t.Error("expected model-a to survive a failed refresh (last-good retention)")
	}
	if _, ok := table.Lookup("model-b"); !ok {
		t.Error("expected model-b to survive a failed refresh (last-good retention)")
	}
}

func TestManager_NeverSucceededSourceContributesNothing(t *testing.T) {
	src := Source{
		Name: "always-down", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "always-down",
		Discover: func(_ context.Context) ([]string, error) {
			return nil, errors.New("connection refused")
		},
	}

	m := NewManager([]Source{src}, 0)
	m.Refresh(context.Background())

	table := m.Snapshot()
	if len(table.IDs()) != 0 {
		t.Errorf("expected empty table for a source that has never succeeded, got %v", table.IDs())
	}
}

func TestManager_SourceShrinkDropsModels(t *testing.T) {
	calls := 0
	src := Source{
		Name: "shrinking", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "shrinking",
		Discover: func(_ context.Context) ([]string, error) {
			calls++
			if calls == 1 {
				return []string{"model-a", "model-b", "model-c"}, nil
			}
			return []string{"model-a"}, nil
		},
	}

	m := NewManager([]Source{src}, 0)
	m.Refresh(context.Background())
	if len(m.Snapshot().IDs()) != 3 {
		t.Fatalf("expected 3 models after first refresh, got %d", len(m.Snapshot().IDs()))
	}

	m.Refresh(context.Background())
	table := m.Snapshot()
	if len(table.IDs()) != 1 {
		t.Fatalf("expected shrink to 1 model, got %d (%v)", len(table.IDs()), table.IDs())
	}
	if _, ok := table.Lookup("model-b"); ok {
		t.Error("expected model-b to be dropped after source shrink")
	}
	if _, ok := table.Lookup("model-a"); !ok {
		t.Error("expected model-a to remain after source shrink")
	}
}

func TestManager_EmptyTableBeforeFirstRefresh(t *testing.T) {
	m := NewManager([]Source{staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})}, 0)
	if len(m.Snapshot().IDs()) != 0 {
		t.Error("expected empty table before Refresh is ever called")
	}
}

// captureLog redirects the standard logger's output for the duration of the
// test and restores it afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(orig)
		log.SetFlags(origFlags)
	})
	return &buf
}

// ---------------------------------------------------------------------------
// Item 6: concurrent per-source discovery, overlapping-refresh guard
// ---------------------------------------------------------------------------

// TestManager_DiscoverySourcesRunConcurrently proves sources are discovered
// in parallel, not serially: two sources that each sleep 150ms must complete
// a single Refresh in well under their combined 300ms if run concurrently.
func TestManager_DiscoverySourcesRunConcurrently(t *testing.T) {
	sleepy := func(name string, ids []string) Source {
		return Source{
			Name: name, Kind: "vllm", Class: ClassSelfHosted, ProviderKey: name,
			Discover: func(ctx context.Context) ([]string, error) {
				select {
				case <-time.After(150 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return ids, nil
			},
		}
	}

	m := NewManager([]Source{sleepy("a", []string{"model-a"}), sleepy("b", []string{"model-b"})}, 0)

	start := time.Now()
	m.Refresh(context.Background())
	elapsed := time.Since(start)

	if elapsed > 280*time.Millisecond {
		t.Errorf("Refresh took %s; expected sources to be discovered concurrently (~150ms), not serially (~300ms)", elapsed)
	}
	if _, ok := m.Snapshot().Lookup("model-a"); !ok {
		t.Error("expected model-a to be discovered")
	}
	if _, ok := m.Snapshot().Lookup("model-b"); !ok {
		t.Error("expected model-b to be discovered")
	}
}

// TestManager_OverlappingRefreshIsSkipped asserts a Refresh call that arrives
// while another is still in flight returns immediately (does not block, does
// not queue a second discovery pass) rather than piling up concurrent
// discovery storms behind a slow source.
func TestManager_OverlappingRefreshIsSkipped(t *testing.T) {
	release := make(chan struct{})
	calls := 0
	var mu sync.Mutex
	src := Source{
		Name: "slow", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "slow",
		Discover: func(_ context.Context) ([]string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			<-release
			return []string{"model-a"}, nil
		},
	}
	m := NewManager([]Source{src}, 0)

	done := make(chan struct{})
	go func() {
		m.Refresh(context.Background())
		close(done)
	}()

	// Give the first Refresh time to enter Discover and take the refreshing flag.
	time.Sleep(30 * time.Millisecond)

	// This second call must return immediately rather than blocking until the
	// first Refresh (and its still-blocked Discover) completes.
	overlapStart := time.Now()
	m.Refresh(context.Background())
	overlapElapsed := time.Since(overlapStart)
	if overlapElapsed > 20*time.Millisecond {
		t.Errorf("overlapping Refresh took %s; expected it to be skipped immediately", overlapElapsed)
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Errorf("expected Discover to have been called exactly once (the overlapping Refresh must be skipped, not queued), got %d", gotCalls)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first Refresh did not complete after release")
	}
}

// ---------------------------------------------------------------------------
// Item 7: route-table change logging
// ---------------------------------------------------------------------------

func TestManager_LogsAddedAndRemovedModelsOnChange(t *testing.T) {
	buf := captureLog(t)

	calls := 0
	src := Source{
		Name: "vllm-default", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "vllm-default",
		Discover: func(_ context.Context) ([]string, error) {
			calls++
			if calls == 1 {
				return []string{"model-a"}, nil
			}
			return []string{"model-b"}, nil
		},
	}
	m := NewManager([]Source{src}, 0)

	m.Refresh(context.Background())
	if !strings.Contains(buf.String(), "added=[model-a(vllm-default)]") {
		t.Errorf("expected an added-model log line on first discovery, got log:\n%s", buf.String())
	}

	buf.Reset()
	m.Refresh(context.Background())
	out := buf.String()
	if !strings.Contains(out, "added=[model-b(vllm-default)]") {
		t.Errorf("expected model-b to be logged as added, got:\n%s", out)
	}
	if !strings.Contains(out, "removed=[model-a(vllm-default)]") {
		t.Errorf("expected model-a to be logged as removed, got:\n%s", out)
	}
}

func TestManager_NoRouteChangeLogWhenUnchanged(t *testing.T) {
	src := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"model-a"})
	m := NewManager([]Source{src}, 0)
	m.Refresh(context.Background())

	buf := captureLog(t)
	m.Refresh(context.Background())
	if strings.Contains(buf.String(), "route table changed") {
		t.Errorf("expected no route-change log line when the routable set is unchanged, got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Item 10: last-good TTL expiry
// ---------------------------------------------------------------------------

func TestManager_LastGoodSurvivesFailureUnderTTL(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	src := Source{
		Name: "flaky", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "flaky",
		Discover: func(_ context.Context) ([]string, error) {
			calls++
			if calls == 1 {
				return []string{"model-a"}, nil
			}
			return nil, errors.New("upstream down")
		},
	}
	m := NewManager([]Source{src}, 60*time.Second)
	m.now = fc.now

	m.Refresh(context.Background())
	fc.t = fc.t.Add(30 * time.Second) // under the 60s TTL
	m.Refresh(context.Background())

	if _, ok := m.Snapshot().Lookup("model-a"); !ok {
		t.Error("expected model-a to remain routable while the failure is under the TTL")
	}
}

// TestManager_LastGoodExpiresAfterTTL is the core item-10 regression guard:
// a source down longer than the TTL must have its models leave the table
// (so requests get a clean 404 model_not_found instead of dialling a
// long-dead backend), and the transition must be logged both ways.
func TestManager_LastGoodExpiresAfterTTL(t *testing.T) {
	buf := captureLog(t)
	fc := &fakeClock{t: time.Unix(0, 0)}
	failing := true
	src := Source{
		Name: "flaky", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "flaky",
		Discover: func(_ context.Context) ([]string, error) {
			if failing {
				return nil, errors.New("upstream down")
			}
			return []string{"model-a"}, nil
		},
	}
	m := NewManager([]Source{src}, 60*time.Second)
	m.now = fc.now

	failing = false
	m.Refresh(context.Background()) // initial success
	if _, ok := m.Snapshot().Lookup("model-a"); !ok {
		t.Fatal("expected model-a after the initial successful discovery")
	}

	failing = true
	fc.t = fc.t.Add(61 * time.Second) // past the 60s TTL
	m.Refresh(context.Background())

	if _, ok := m.Snapshot().Lookup("model-a"); ok {
		t.Fatal("expected model-a to be dropped once the source's failure exceeds the TTL")
	}
	if len(m.Snapshot().IDs()) != 0 {
		t.Errorf("expected an empty table after TTL expiry, got %v", m.Snapshot().IDs())
	}
	expiryLog := buf.String()
	if !strings.Contains(expiryLog, "WARNING flaky") || !strings.Contains(expiryLog, "leaving the route table") {
		t.Errorf("expected a WARNING log line on TTL expiry, got:\n%s", expiryLog)
	}
	if !strings.Contains(expiryLog, "removed=[model-a(flaky)]") {
		t.Errorf("expected the route-change log (item 7) to fire on TTL expiry, got:\n%s", expiryLog)
	}

	// Recovery: source succeeds again -> model-a is restored and both the
	// recovery WARNING and the route-change "added" log fire.
	buf.Reset()
	failing = false
	fc.t = fc.t.Add(1 * time.Second)
	m.Refresh(context.Background())

	if _, ok := m.Snapshot().Lookup("model-a"); !ok {
		t.Fatal("expected model-a to be restored once the source recovers")
	}
	recoveryLog := buf.String()
	if !strings.Contains(recoveryLog, "flaky: recovered") {
		t.Errorf("expected a recovery log line, got:\n%s", recoveryLog)
	}
	if !strings.Contains(recoveryLog, "added=[model-a(flaky)]") {
		t.Errorf("expected the route-change log (item 7) to fire on recovery, got:\n%s", recoveryLog)
	}
}

// TestManager_NeverSucceededSourceNeverExpires guards against a source that
// has never had a single successful discovery being (incorrectly) logged as
// "expired" — it already contributes nothing, and there is no "last success"
// to measure a TTL against.
func TestManager_NeverSucceededSourceNeverExpires(t *testing.T) {
	buf := captureLog(t)
	fc := &fakeClock{t: time.Unix(0, 0)}
	src := Source{
		Name: "always-down", Kind: "vllm", Class: ClassSelfHosted, ProviderKey: "always-down",
		Discover: func(_ context.Context) ([]string, error) {
			return nil, errors.New("connection refused")
		},
	}
	m := NewManager([]Source{src}, 60*time.Second)
	m.now = fc.now

	m.Refresh(context.Background())
	fc.t = fc.t.Add(1 * time.Hour)
	m.Refresh(context.Background())

	if strings.Contains(buf.String(), "WARNING always-down") {
		t.Errorf("a source that has never succeeded must not log a TTL-expiry WARNING, got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Item 8: Status() for /health
// ---------------------------------------------------------------------------

func TestManager_Status(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1000, 0)}
	okSrc := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"model-a", "model-b"})
	failSrc := Source{
		Name: "bedrock", Kind: "bedrock", Class: ClassCloudManaged, ProviderKey: "bedrock",
		Discover: func(_ context.Context) ([]string, error) {
			return nil, errors.New("access denied")
		},
	}
	m := NewManager([]Source{okSrc, failSrc}, 0)
	m.now = fc.now

	fc.t = fc.t.Add(5 * time.Second)
	m.Refresh(context.Background())

	status := m.Status()
	if status.Models != 2 {
		t.Errorf("Status.Models = %d, want 2", status.Models)
	}

	okStatus, ok := status.Sources["vllm-default"]
	if !ok {
		t.Fatal("expected vllm-default in Status.Sources")
	}
	if okStatus.Models != 2 {
		t.Errorf("vllm-default.Models = %d, want 2", okStatus.Models)
	}
	if okStatus.LastSuccessSecondsAgo == nil || *okStatus.LastSuccessSecondsAgo != 0 {
		t.Errorf("vllm-default.LastSuccessSecondsAgo = %v, want 0", okStatus.LastSuccessSecondsAgo)
	}
	if okStatus.LastError != "" {
		t.Errorf("vllm-default.LastError = %q, want empty", okStatus.LastError)
	}

	failStatus, ok := status.Sources["bedrock"]
	if !ok {
		t.Fatal("expected bedrock in Status.Sources")
	}
	if failStatus.Models != 0 {
		t.Errorf("bedrock.Models = %d, want 0 (never succeeded)", failStatus.Models)
	}
	if failStatus.LastSuccessSecondsAgo != nil {
		t.Errorf("bedrock.LastSuccessSecondsAgo = %v, want nil (never succeeded)", failStatus.LastSuccessSecondsAgo)
	}
	if failStatus.LastError != "access denied" {
		t.Errorf("bedrock.LastError = %q, want %q", failStatus.LastError, "access denied")
	}
}

func TestManager_ConcurrentRefreshAndSnapshot(t *testing.T) {
	src := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})
	m := NewManager([]Source{src}, 0)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.Refresh(context.Background())
		}()
		go func() {
			defer wg.Done()
			_ = m.Snapshot().IDs()
		}()
	}
	wg.Wait()
}
