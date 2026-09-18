package router

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeCompleteProvider struct {
	name string
	kind string

	response *provider.ChatResponse
	err      error
	delay    time.Duration
	calls    int
}

func (p *fakeCompleteProvider) Name() string { return p.name }
func (p *fakeCompleteProvider) Kind() string { return p.kind }

func (p *fakeCompleteProvider) Complete(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.calls++
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.response, nil
}

func (p *fakeCompleteProvider) Stream(_ context.Context, _ provider.ChatRequest, _ io.Writer) (*provider.StreamStats, error) {
	return nil, errors.New("not implemented")
}

type fakeRegistry map[string]provider.Provider

func (f fakeRegistry) Get(key string) (provider.Provider, bool) {
	p, ok := f[key]
	return p, ok
}

type fakeRoutes struct{ table *routing.Table }

func (f fakeRoutes) Snapshot() *routing.Table { return f.table }

type fakePolicy struct {
	snap   policy.Snapshot
	loaded bool
}

func (f fakePolicy) Current() (policy.Snapshot, bool) { return f.snap, f.loaded }

// classifierAlias is a routed alias configured to use a model classifier
// pointed at "router-model".
func classifierAlias() policy.AliasDef {
	return policy.AliasDef{
		Kind:            policy.AliasKindRouted,
		DefaultRoute:    "general",
		ClassifierKind:  policy.ClassifierKindModel,
		ClassifierModel: "router-model",
		Routes: []policy.RouteDef{
			{Name: "code", Description: "code questions", Target: "qwen-coder"},
			{Name: "general", Description: "everything else", Target: "llama-3-8b"},
		},
	}
}

func validClassifierEnv(prov provider.Provider) (fakeRegistry, fakeRoutes, fakePolicy) {
	table := routing.NewTable(map[string]routing.Route{
		"router-model": {ProviderKey: "router-provider", Kind: "vllm", Class: routing.ClassSelfHosted},
	})
	return fakeRegistry{"router-provider": prov},
		fakeRoutes{table: table},
		fakePolicy{snap: policy.Snapshot{Enabled: map[string]bool{"router-model": true}}, loaded: true}
}

// ---------------------------------------------------------------------------
// classifierValid — pure
// ---------------------------------------------------------------------------

func TestClassifierValid(t *testing.T) {
	table := routing.NewTable(map[string]routing.Route{
		"self-hosted-model": {ProviderKey: "p", Class: routing.ClassSelfHosted},
		"cloud-model":       {ProviderKey: "p", Class: routing.ClassCloudManaged},
	})
	snap := policy.Snapshot{Enabled: map[string]bool{"self-hosted-model": true, "cloud-model": true}}

	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"self-hosted and enabled", "self-hosted-model", true},
		{"cloud-managed is invalid regardless of enabled", "cloud-model", false},
		{"unknown model", "does-not-exist", false},
		{"empty model id", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifierValid(table, snap, tc.model); got != tc.want {
				t.Errorf("classifierValid(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}

	notEnabledSnap := policy.Snapshot{Enabled: map[string]bool{}}
	if classifierValid(table, notEnabledSnap, "self-hosted-model") {
		t.Error("expected a self-hosted but not-enabled model to be invalid")
	}
}

// ---------------------------------------------------------------------------
// parseArchRouterOutput — pure
// ---------------------------------------------------------------------------

func TestParseArchRouterOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"exact JSON", `{"route": "code"}`, "code", true},
		{"surrounding text", "sure, here you go: {\"route\":\"general\"} thanks", "general", true},
		{"other sentinel", `{"route":"other"}`, "other", true},
		{"empty route field", `{"route":""}`, "", false},
		{"no JSON at all", "code", "", false},
		{"unbalanced braces", `{"route":"code"`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseArchRouterOutput(tc.raw)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseArchRouterOutput(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// decideFromModelResult — pure Decide function
// ---------------------------------------------------------------------------

func TestDecideFromModelResult(t *testing.T) {
	alias := classifierAlias()

	t.Run("happy path exact match", func(t *testing.T) {
		d, err := decideFromModelResult(alias, `{"route":"code"}`, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "code" || d.Reason != "model:code" {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		d, err := decideFromModelResult(alias, `{"route":"CODE"}`, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "code" {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("unparseable output", func(t *testing.T) {
		d, err := decideFromModelResult(alias, "I'm not sure, maybe try the code route?", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "general" || d.Reason != ReasonFallbackUnparseable {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("other sentinel is unparseable", func(t *testing.T) {
		d, _ := decideFromModelResult(alias, `{"route":"other"}`, nil)
		if d.RouteName != "general" || d.Reason != ReasonFallbackUnparseable {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		d, err := decideFromModelResult(alias, "", context.DeadlineExceeded)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "general" || d.Reason != ReasonFallbackTimeout {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("other transport error also falls back to timeout bucket", func(t *testing.T) {
		d, err := decideFromModelResult(alias, "", errors.New("connection refused"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "general" || d.Reason != ReasonFallbackTimeout {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("canceled aborts", func(t *testing.T) {
		d, err := decideFromModelResult(alias, "", context.Canceled)
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("expected ErrAborted, got %v", err)
		}
		if d != (Decision{}) {
			t.Errorf("expected zero Decision on abort, got %+v", d)
		}
	})
}

// ---------------------------------------------------------------------------
// ModelClassifier.Classify — integration of the above with fakes
// ---------------------------------------------------------------------------

func TestModelClassifier_HappyPath(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm", response: &provider.ChatResponse{Content: `{"route":"code"}`, PromptTokens: 40, CompletionTokens: 3}}
	reg, routes, pol := validClassifierEnv(prov)
	c := NewModelClassifier(reg, routes, pol, 500*time.Millisecond, 5, 30*time.Second)

	d, err := c.Classify(context.Background(), classifierAlias(), Input{Text: "please fix this stack trace"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "code" || d.Reason != "model:code" {
		t.Fatalf("got %+v", d)
	}
	if d.ModelCall == nil {
		t.Fatal("expected ModelCall to be populated for a model classifier call")
	}
	if d.ModelCall.Model != "router-model" || d.ModelCall.Provider != "vllm" || d.ModelCall.StatusCode != 200 {
		t.Errorf("got %+v", d.ModelCall)
	}
	if d.ModelCall.PromptTokens == nil || *d.ModelCall.PromptTokens != 40 {
		t.Errorf("prompt tokens: got %+v", d.ModelCall.PromptTokens)
	}
	if prov.calls != 1 {
		t.Errorf("expected exactly one Complete call, got %d", prov.calls)
	}
}

func TestModelClassifier_UnparseableFallsBackToDefault(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm", response: &provider.ChatResponse{Content: "not json at all"}}
	reg, routes, pol := validClassifierEnv(prov)
	c := NewModelClassifier(reg, routes, pol, 500*time.Millisecond, 5, 30*time.Second)

	d, err := c.Classify(context.Background(), classifierAlias(), Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "general" || d.Reason != ReasonFallbackUnparseable {
		t.Errorf("got %+v", d)
	}
	if d.ModelCall == nil {
		t.Fatal("expected ModelCall to still be populated — the model DID see the prompt, it just answered badly")
	}
}

func TestModelClassifier_TimeoutFallsBackAndRecordsFailure(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm", delay: 50 * time.Millisecond}
	reg, routes, pol := validClassifierEnv(prov)
	c := NewModelClassifier(reg, routes, pol, 5*time.Millisecond, 5, 30*time.Second)

	d, err := c.Classify(context.Background(), classifierAlias(), Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "general" || d.Reason != ReasonFallbackTimeout {
		t.Errorf("got %+v", d)
	}
	if d.ModelCall == nil || d.ModelCall.StatusCode != 504 {
		t.Errorf("got %+v", d.ModelCall)
	}
}

func TestModelClassifier_CanceledAborts(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm", delay: time.Second}
	reg, routes, pol := validClassifierEnv(prov)
	c := NewModelClassifier(reg, routes, pol, time.Second, 5, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Classify(ctx, classifierAlias(), Input{Text: "hello"})
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected ErrAborted for a pre-canceled context, got %v", err)
	}
}

func TestModelClassifier_InvalidClassifierDegrades(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm"}

	t.Run("classifier model not self-hosted", func(t *testing.T) {
		table := routing.NewTable(map[string]routing.Route{
			"router-model": {ProviderKey: "router-provider", Class: routing.ClassCloudManaged},
		})
		pol := fakePolicy{snap: policy.Snapshot{Enabled: map[string]bool{"router-model": true}}, loaded: true}
		c := NewModelClassifier(fakeRegistry{"router-provider": prov}, fakeRoutes{table: table}, pol, time.Second, 5, 30*time.Second)

		d, err := c.Classify(context.Background(), classifierAlias(), Input{Text: "hello"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.Reason != ReasonFallbackClassifierInvalid || d.ModelCall != nil {
			t.Errorf("got %+v", d)
		}
		if prov.calls != 0 {
			t.Errorf("classifier model must never be called when invalid, got %d calls", prov.calls)
		}
	})

	t.Run("classifier model not enabled", func(t *testing.T) {
		table := routing.NewTable(map[string]routing.Route{
			"router-model": {ProviderKey: "router-provider", Class: routing.ClassSelfHosted},
		})
		pol := fakePolicy{snap: policy.Snapshot{Enabled: map[string]bool{}}, loaded: true}
		c := NewModelClassifier(fakeRegistry{"router-provider": prov}, fakeRoutes{table: table}, pol, time.Second, 5, 30*time.Second)

		d, _ := c.Classify(context.Background(), classifierAlias(), Input{Text: "hello"})
		if d.Reason != ReasonFallbackClassifierInvalid {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("policy never loaded", func(t *testing.T) {
		table := routing.NewTable(map[string]routing.Route{
			"router-model": {ProviderKey: "router-provider", Class: routing.ClassSelfHosted},
		})
		pol := fakePolicy{loaded: false}
		c := NewModelClassifier(fakeRegistry{"router-provider": prov}, fakeRoutes{table: table}, pol, time.Second, 5, 30*time.Second)

		d, _ := c.Classify(context.Background(), classifierAlias(), Input{Text: "hello"})
		if d.Reason != ReasonFallbackClassifierInvalid {
			t.Errorf("got %+v", d)
		}
	})
}

func TestModelClassifier_BreakerOpensAfterConsecutiveFailuresThenCloses(t *testing.T) {
	prov := &fakeCompleteProvider{name: "router-provider", kind: "vllm", err: errors.New("upstream 500")}
	reg, routes, pol := validClassifierEnv(prov)
	c := NewModelClassifier(reg, routes, pol, time.Second, 2, 20*time.Millisecond)
	alias := classifierAlias()

	// Two consecutive failures trip the breaker (threshold=2).
	for i := 0; i < 2; i++ {
		d, err := c.Classify(context.Background(), alias, Input{Text: "hello"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.Reason != ReasonFallbackTimeout {
			t.Fatalf("call %d: got %+v", i, d)
		}
	}
	if prov.calls != 2 {
		t.Fatalf("expected 2 calls before the breaker opens, got %d", prov.calls)
	}

	// Breaker is now open: the next call must not reach the provider at all.
	d, err := c.Classify(context.Background(), alias, Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Reason != ReasonFallbackCircuitOpen || d.ModelCall != nil {
		t.Fatalf("got %+v", d)
	}
	if prov.calls != 2 {
		t.Fatalf("classifier must be skipped while the breaker is open, got %d calls", prov.calls)
	}
	if open, failures := c.Status(); !open || failures != 2 {
		t.Errorf("Status() = open=%v failures=%d, want open=true failures=2", open, failures)
	}

	// After the open window elapses, the breaker closes and a healthy call
	// resets the failure count.
	time.Sleep(30 * time.Millisecond)
	prov.err = nil
	prov.response = &provider.ChatResponse{Content: `{"route":"general"}`}
	d, err = c.Classify(context.Background(), alias, Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "general" || d.Reason != "model:general" {
		t.Fatalf("got %+v", d)
	}
	if open, failures := c.Status(); open || failures != 0 {
		t.Errorf("Status() after recovery = open=%v failures=%d, want open=false failures=0", open, failures)
	}
}
