package main

import (
	"strings"
	"testing"
)

func TestRender_GroupsByServiceAndAppliesFilterNotes(t *testing.T) {
	entries := []routeEntry{
		{Service: "api", Method: "GET", Path: "/api/v1/activity", Auth: "admin", Surface: "ui"},
		{Service: "api", Method: "GET", Path: "/health", Auth: "public", Surface: "ui"},
		{Service: "gateway", Method: "GET", Path: "/v1/policy/effective", Auth: "service", Surface: "machine", Reason: "service-key only, never called by a browser"},
	}
	md := render(entries)

	if !strings.Contains(md, "## api") || !strings.Contains(md, "## gateway") {
		t.Fatalf("expected both service sections, got:\n%s", md)
	}
	if !strings.Contains(md, "filters: actor_id, event_type, since, until, limit, offset") {
		t.Fatalf("expected the hand-written filter note for /api/v1/activity, got:\n%s", md)
	}
	if !strings.Contains(md, "surface: machine (service-key only, never called by a browser)") {
		t.Fatalf("expected the machine reason inlined, got:\n%s", md)
	}
}

func TestRender_NoFilterNoteOmitsFiltersSuffix(t *testing.T) {
	entries := []routeEntry{
		{Service: "api", Method: "DELETE", Path: "/api/v1/users/:id", Auth: "admin", Surface: "ui"},
	}
	md := render(entries)
	if strings.Contains(md, "filters:") {
		t.Fatalf("expected no filters suffix for an endpoint with no filterNotes entry, got:\n%s", md)
	}
}
