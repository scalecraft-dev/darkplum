package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
)

func semanticAlias() policy.AliasDef {
	return policy.AliasDef{
		Kind:           policy.AliasKindRouted,
		DefaultRoute:   "general",
		ClassifierKind: policy.ClassifierKindSemanticRouter,
		Routes: []policy.RouteDef{
			{Name: "code", Description: "code questions", Target: "qwen-coder"},
			{Name: "general", Description: "everything else", Target: "llama-3-8b"},
		},
	}
}

// ---------------------------------------------------------------------------
// decideFromSemanticResult — pure
// ---------------------------------------------------------------------------

func TestDecideFromSemanticResult(t *testing.T) {
	alias := semanticAlias()

	t.Run("happy path", func(t *testing.T) {
		d, err := decideFromSemanticResult(alias, "code", 0.9, 0.6, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "code" || d.Reason != "semantic:code" || d.ModelCall != nil {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("case-insensitive category match", func(t *testing.T) {
		d, _ := decideFromSemanticResult(alias, "CODE", 0.9, 0.6, nil)
		if d.RouteName != "code" {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("below confidence threshold", func(t *testing.T) {
		d, err := decideFromSemanticResult(alias, "code", 0.59, 0.6, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "general" || d.Reason != ReasonFallbackLowConfidence {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("unknown category", func(t *testing.T) {
		d, _ := decideFromSemanticResult(alias, "astrology", 0.9, 0.6, nil)
		if d.RouteName != "general" || d.Reason != ReasonFallbackUnknownCategory {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		d, err := decideFromSemanticResult(alias, "", 0, 0.6, context.DeadlineExceeded)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.RouteName != "general" || d.Reason != ReasonFallbackTimeout {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("canceled aborts", func(t *testing.T) {
		_, err := decideFromSemanticResult(alias, "", 0, 0.6, context.Canceled)
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("expected ErrAborted, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// SemanticRouterClassifier.Classify — httptest integration
// ---------------------------------------------------------------------------

func TestSemanticRouterClassifier_Unconfigured(t *testing.T) {
	c := NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)

	d, err := c.Classify(context.Background(), semanticAlias(), Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Reason != ReasonFallbackClassifierInvalid {
		t.Errorf("got %+v", d)
	}
	if status := c.HealthStatus(context.Background()); status != "unconfigured" {
		t.Errorf("HealthStatus() = %q, want unconfigured", status)
	}
}

func TestSemanticRouterClassifier_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// HealthStatus's reachability probe.
			return
		}
		if r.URL.Path != "/api/v1/classify/intent" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"classification":{"category":"code","confidence":0.95}}`))
	}))
	defer srv.Close()

	c := NewSemanticRouterClassifier(srv.URL, 0.6, time.Second, 5, 30*time.Second)
	d, err := c.Classify(context.Background(), semanticAlias(), Input{Text: "fix my bug"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "code" || d.Reason != "semantic:code" {
		t.Errorf("got %+v", d)
	}
	if d.ModelCall != nil {
		t.Error("semantic router classification must never produce a ModelCall (no second audit row — not an LLM inference)")
	}
	if status := c.HealthStatus(context.Background()); status != "configured" {
		t.Errorf("HealthStatus() = %q, want configured", status)
	}
}

func TestSemanticRouterClassifier_LowConfidenceFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"classification":{"category":"code","confidence":0.2}}`))
	}))
	defer srv.Close()

	c := NewSemanticRouterClassifier(srv.URL, 0.6, time.Second, 5, 30*time.Second)
	d, _ := c.Classify(context.Background(), semanticAlias(), Input{Text: "hello"})
	if d.RouteName != "general" || d.Reason != ReasonFallbackLowConfidence {
		t.Errorf("got %+v", d)
	}
}

func TestSemanticRouterClassifier_UnreachableFallsBackAndReportsUnreachable(t *testing.T) {
	// A URL nothing listens on: connection refused.
	c := NewSemanticRouterClassifier("http://127.0.0.1:1", 0.6, 50*time.Millisecond, 5, 30*time.Second)

	d, err := c.Classify(context.Background(), semanticAlias(), Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "general" || d.Reason != ReasonFallbackTimeout {
		t.Errorf("got %+v", d)
	}
	if status := c.HealthStatus(context.Background()); status != "unreachable" {
		t.Errorf("HealthStatus() = %q, want unreachable", status)
	}
}

func TestSemanticRouterClassifier_CanceledAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := NewSemanticRouterClassifier(srv.URL, 0.6, time.Second, 5, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Classify(ctx, semanticAlias(), Input{Text: "hello"})
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected ErrAborted, got %v", err)
	}
}

func TestSemanticRouterClassifier_BreakerOpensAndSkipsCalls(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewSemanticRouterClassifier(srv.URL, 0.6, time.Second, 2, time.Minute)
	alias := semanticAlias()

	for i := 0; i < 2; i++ {
		if _, err := c.Classify(context.Background(), alias, Input{Text: "hello"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls before the breaker opens, got %d", calls)
	}

	d, err := c.Classify(context.Background(), alias, Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Reason != ReasonFallbackCircuitOpen {
		t.Fatalf("got %+v", d)
	}
	if calls != 2 {
		t.Fatalf("classifier must be skipped while the breaker is open, got %d calls", calls)
	}
}
