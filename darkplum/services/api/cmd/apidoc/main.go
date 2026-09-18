// Command apidoc renders docs/API.md from docs/api-routes.json (the golden
// route inventory shared with the gateway; see DESIGN4's Parity section and
// cmd/route_inventory_test.go, which enforces that the golden file always
// matches the api's actual gin route table).
//
// Run from services/api: `go run ./cmd/apidoc`. It writes docs/API.md,
// one line per endpoint, in golden order, annotated with the query filters
// each endpoint accepts where documented in the filterNotes map below — a
// short hand-written table rather than reflection over handler source, since
// query-param names live inside handler bodies (c.Query("...") calls) with
// no machine-readable declaration to introspect.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// routeEntry mirrors one element of docs/api-routes.json.
type routeEntry struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Auth    string `json:"auth"`
	Surface string `json:"surface"`
	Reason  string `json:"reason,omitempty"`
}

// filterNotes documents the query-string filters accepted by list/export
// endpoints, keyed by "SERVICE METHOD PATH" (the same key shape used
// throughout this file). Endpoints not listed here simply render without a
// "Filters:" line -- most of them (single-resource GET/PUT/DELETE, writes)
// have none. Kept hand-written and in the SAME package as the golden-diff
// logic's key format, per DESIGN4's contract, rather than derived by
// scanning handler source for c.Query(...) calls, which would require a Go
// AST walk for a one-page doc with a low rate of change.
var filterNotes = map[string]string{
	"api GET /api/v1/activity":                  "actor_id, event_type, since, until, limit, offset",
	"api GET /api/v1/activity/export":           "actor_id, event_type, since, until",
	"api GET /api/v1/usage":                     "user_id, api_key_id, model, purpose, since, until, limit, offset",
	"api GET /api/v1/usage/export":              "user_id, api_key_id, model, purpose, since, until",
	"api GET /api/v1/usage/stats":               "interval, user_id, api_key_id, since, until",
	"api GET /api/v1/usage/summary":             "since, until",
	"api GET /api/v1/usage/user-summary":        "since, until",
	"api GET /api/v1/usage/key-summary":         "since, until",
	"api GET /api/v1/conversations":             "limit",
	"api GET /api/v1/conversations/:id":         "format=export",
	"api GET /api/v1/routing-policy/principals": "limit (max 200), offset",
}

func main() {
	golden := flag.String("golden", "", "path to docs/api-routes.json (default: auto-detected relative to cwd)")
	out := flag.String("out", "", "path to write docs/API.md (default: alongside the detected golden file)")
	flag.Parse()

	goldenPath := *golden
	if goldenPath == "" {
		found, err := findGolden()
		if err != nil {
			fmt.Fprintln(os.Stderr, "apidoc:", err)
			os.Exit(1)
		}
		goldenPath = found
	}

	data, err := os.ReadFile(goldenPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apidoc: read golden file:", err)
		os.Exit(1)
	}
	var entries []routeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		fmt.Fprintln(os.Stderr, "apidoc: decode golden file:", err)
		os.Exit(1)
	}

	outPath := *out
	if outPath == "" {
		outPath = strings.TrimSuffix(goldenPath, "api-routes.json") + "API.md"
	}

	md := render(entries)
	if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "apidoc: write output:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "apidoc: wrote", outPath)
}

// findGolden looks for docs/api-routes.json at a handful of relative depths
// so `go run ./cmd/apidoc` works whether invoked from services/api (the
// documented location) or the repo root.
func findGolden() (string, error) {
	candidates := []string{
		"docs/api-routes.json",
		"../docs/api-routes.json",
		"../../docs/api-routes.json",
		"../../../docs/api-routes.json",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find docs/api-routes.json near the current directory; pass -golden explicitly")
}

// render builds docs/API.md's full text: one section per service, one line
// per endpoint, in golden order (not re-sorted, so the doc's ordering
// matches the golden file an editor already has open).
func render(entries []routeEntry) string {
	var b strings.Builder
	b.WriteString("# Darkplum API route inventory\n\n")
	b.WriteString("Generated from docs/api-routes.json by cmd/apidoc — do not hand-edit; regenerate with `go run ./cmd/apidoc` from services/api after changing routes.\n\n")

	services := []string{"api", "gateway"}
	for _, svc := range services {
		var rows []routeEntry
		for _, e := range entries {
			if e.Service == svc {
				rows = append(rows, e)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", svc)
		for _, e := range rows {
			key := svc + " " + e.Method + " " + e.Path
			line := fmt.Sprintf("- `%s %s` — auth: %s, surface: %s", e.Method, e.Path, e.Auth, e.Surface)
			if e.Surface == "machine" && e.Reason != "" {
				line += fmt.Sprintf(" (%s)", e.Reason)
			}
			if filters, ok := filterNotes[key]; ok {
				line += fmt.Sprintf(" — filters: %s", filters)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
