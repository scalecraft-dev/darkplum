package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
)

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
	m := NewManager([]Source{brk, vllm})
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

	m := NewManager([]Source{first, second})
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

	m := NewManager([]Source{src})
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

	m := NewManager([]Source{src})
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

	m := NewManager([]Source{src})
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
	m := NewManager([]Source{staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})})
	if len(m.Snapshot().IDs()) != 0 {
		t.Error("expected empty table before Refresh is ever called")
	}
}

func TestManager_ConcurrentRefreshAndSnapshot(t *testing.T) {
	src := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})
	m := NewManager([]Source{src})

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
