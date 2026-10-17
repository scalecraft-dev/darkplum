package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The gateway's HTTP surface is pinned by docs/api-routes.json (service:
// "gateway"), the same golden the api and the web client are checked against.
// A route added to buildRouter without a golden entry fails here; so does a
// stale entry. Every gateway route is machine-facing (the browser only ever
// talks to the api), so each entry must carry a reason.

type goldenRoute struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Surface string `json:"surface"`
	Reason  string `json:"reason"`
}

func loadGatewayGolden(t *testing.T) []goldenRoute {
	t.Helper()
	// cmd/ -> gateway/ -> services/ -> darkplum/ -> docs/api-routes.json
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api-routes.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var all []goldenRoute
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	var out []goldenRoute
	for _, e := range all {
		if e.Service == "gateway" {
			out = append(out, e)
		}
	}
	return out
}

func TestRouteInventoryMatchesGolden(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)
	registered := make(map[string]bool)
	for _, ri := range router.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}

	golden := loadGatewayGolden(t)
	if len(golden) == 0 {
		t.Fatal("golden has no service=gateway entries")
	}
	declared := make(map[string]bool)
	for _, e := range golden {
		key := e.Method + " " + e.Path
		declared[key] = true
		if e.Surface != "machine" {
			t.Errorf("golden entry %s is surface=%q; every gateway route is machine-facing (the browser talks to the api)", key, e.Surface)
		}
		if e.Reason == "" {
			t.Errorf("golden entry %s has no reason", key)
		}
		if !registered[key] {
			t.Errorf("golden declares %s but buildRouter does not register it", key)
		}
	}
	for key := range registered {
		if !declared[key] {
			t.Errorf("buildRouter registers %s but docs/api-routes.json (service=gateway) does not declare it", key)
		}
	}
}
