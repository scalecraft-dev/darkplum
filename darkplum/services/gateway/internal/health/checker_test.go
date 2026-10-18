package health_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/health"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func alwaysOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// modelsServer returns a server that responds with a /v1/models-shaped JSON
// body. Pass a non-empty ids slice for a healthy response; pass nil/empty for
// the "reachable but no models" (degraded) case.
func modelsServer(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type modelEntry struct {
			ID string `json:"id"`
		}
		type resp struct {
			Object string       `json:"object"`
			Data   []modelEntry `json:"data"`
		}
		entries := make([]modelEntry, 0, len(ids))
		for _, id := range ids {
			entries = append(entries, modelEntry{ID: id})
		}
		_ = json.NewEncoder(w).Encode(resp{Object: "list", Data: entries})
	}))
}

func always503Server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestChecker_AllOK(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "vllm-default", ProbeURL: srv.URL + "/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["vllm-default"]; got != health.StatusOK {
		t.Errorf("expected ok, got %q", got)
	}
}

func TestChecker_UnreachableHost(t *testing.T) {
	// Nothing listening on this port.
	c := health.NewChecker([]health.Entry{
		{Name: "lmstudio-dev", ProbeURL: "http://127.0.0.1:19998/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["lmstudio-dev"]; got != health.StatusUnreachable {
		t.Errorf("expected unreachable, got %q", got)
	}
}

func TestChecker_Non2xxIsUnreachable(t *testing.T) {
	srv := always503Server(t)
	defer srv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "vllm-default", ProbeURL: srv.URL + "/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["vllm-default"]; got != health.StatusUnreachable {
		t.Errorf("expected unreachable for 503, got %q", got)
	}
}

func TestChecker_MultipleUpstreams(t *testing.T) {
	okSrv := alwaysOKServer(t)
	defer okSrv.Close()
	downSrv := always503Server(t)
	defer downSrv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "vllm-default", ProbeURL: okSrv.URL + "/v1/models"},
		{Name: "lmstudio-dev", ProbeURL: downSrv.URL + "/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["vllm-default"]; got != health.StatusOK {
		t.Errorf("vllm-default: expected ok, got %q", got)
	}
	if got := status["lmstudio-dev"]; got != health.StatusUnreachable {
		t.Errorf("lmstudio-dev: expected unreachable, got %q", got)
	}
}

func TestChecker_EmptyUpstreams(t *testing.T) {
	c := health.NewChecker(nil)
	status := c.Check(context.Background())
	if len(status) != 0 {
		t.Errorf("expected empty map for no upstreams, got %v", status)
	}
}

// TestChecker_ResultIsCached verifies that a second Check() call within the TTL
// returns the cached result without issuing a new HTTP request. We confirm this
// by shutting down the probe server between calls: the second call must still
// return ok (from cache).
func TestChecker_ResultIsCached(t *testing.T) {
	var hitCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitCount++
		w.WriteHeader(http.StatusOK)
	}))

	c := health.NewChecker([]health.Entry{
		{Name: "vllm-default", ProbeURL: srv.URL + "/v1/models"},
	})

	// First call — probes the server.
	status := c.Check(context.Background())
	if status["vllm-default"] != health.StatusOK {
		t.Fatalf("first check: expected ok, got %q", status["vllm-default"])
	}
	if hitCount != 1 {
		t.Fatalf("expected 1 probe hit after first check, got %d", hitCount)
	}

	// Shut the server down. A live probe would now return unreachable.
	srv.Close()

	// Second call within the TTL window — must return cached ok without probing.
	status = c.Check(context.Background())
	if status["vllm-default"] != health.StatusOK {
		t.Errorf("second check (cached): expected ok (from cache), got %q", status["vllm-default"])
	}
	if hitCount != 1 {
		t.Errorf("expected still 1 probe hit (cache should have served), got %d", hitCount)
	}
}

// TestChecker_NoModels verifies that an upstream returning a well-formed but
// empty /v1/models list is reported as StatusNoModels (degraded), not ok.
// This is the "NetworkPolicy incident" case: the process is alive but has
// not loaded any models yet.
func TestChecker_NoModels(t *testing.T) {
	srv := modelsServer(t, nil) // empty data array
	defer srv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "lmstudio-dev", ProbeURL: srv.URL + "/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["lmstudio-dev"]; got != health.StatusNoModels {
		t.Errorf("expected %q for empty model list, got %q", health.StatusNoModels, got)
	}
}

// TestChecker_WithModels verifies that an upstream returning a populated
// /v1/models list is reported as StatusOK.
func TestChecker_WithModels(t *testing.T) {
	srv := modelsServer(t, []string{"llama-3-8b", "mistral-7b"})
	defer srv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "vllm-default", ProbeURL: srv.URL + "/v1/models"},
	})

	status := c.Check(context.Background())
	if got := status["vllm-default"]; got != health.StatusOK {
		t.Errorf("expected ok for populated model list, got %q", got)
	}
}

// TestChecker_ProbeDoesNotBlockBeyondTimeout verifies that a probe against a
// very slow upstream completes within a reasonable upper bound (~2s), not forever.
func TestChecker_ProbeDoesNotBlockBeyondTimeout(t *testing.T) {
	// Server that sleeps longer than the probe timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep until the request context is cancelled (i.e. probe times out).
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := health.NewChecker([]health.Entry{
		{Name: "slow-upstream", ProbeURL: srv.URL + "/v1/models"},
	})

	start := time.Now()
	status := c.Check(context.Background())
	elapsed := time.Since(start)

	if status["slow-upstream"] != health.StatusUnreachable {
		t.Errorf("slow upstream: expected unreachable after timeout, got %q", status["slow-upstream"])
	}
	// Must finish well under 3s. The probe timeout is 1.5s; allow 1s headroom.
	if elapsed > 3*time.Second {
		t.Errorf("probe took too long: %v (want < 3s)", elapsed)
	}
}
