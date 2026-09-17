package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPModelSource_SuccessReturnsIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-3-8b"},{"id":"mistral-7b"}]}`))
	}))
	defer srv.Close()

	src := NewHTTPModelSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", srv.URL)
	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}
}

// TestHTTPModelSource_CleanEmptyResponseIsASuccessfulShrink verifies that a
// well-formed 200 response with zero models is treated as a successful
// discovery of zero models (not an error) — the caller (Manager) is
// responsible for the last-good retention semantics; the source itself must
// distinguish "reachable but empty" from "unreachable".
func TestHTTPModelSource_CleanEmptyResponseIsASuccessfulShrink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	src := NewHTTPModelSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", srv.URL)
	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("expected a clean empty response to be a success, got error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected zero ids, got %v", ids)
	}
}

func TestHTTPModelSource_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	src := NewHTTPModelSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", srv.URL)
	if _, err := src.Discover(context.Background()); err == nil {
		t.Fatal("expected a non-200 response to be reported as an error")
	}
}

func TestHTTPModelSource_UnreachableIsAnError(t *testing.T) {
	src := NewHTTPModelSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", "http://127.0.0.1:19997")
	if _, err := src.Discover(context.Background()); err == nil {
		t.Fatal("expected an unreachable upstream to be reported as an error")
	}
}

func TestHTTPModelSource_ThroughManagerKeepsLastGoodOnOutage(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-3-8b"}]}`))
	}))
	defer srv.Close()

	src := NewHTTPModelSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", srv.URL)
	m := NewManager([]Source{src})
	m.Refresh(context.Background())
	if _, ok := m.Snapshot().Lookup("llama-3-8b"); !ok {
		t.Fatal("expected llama-3-8b to be discovered while upstream is up")
	}

	up = false
	m.Refresh(context.Background())
	if _, ok := m.Snapshot().Lookup("llama-3-8b"); !ok {
		t.Error("expected llama-3-8b to survive an outage (last-good retention)")
	}
}
