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
	"api GET /api/v1/usage":                     "user_id, api_key_id, model, purpose, denial_code (a refusal code, or `any`), since, until, limit, offset",
	"api GET /api/v1/usage/export":              "user_id, api_key_id, model, purpose, denial_code, since, until",
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

// preamble is the fixed text above the route inventory: what each auth value
// means in practice (in particular that admin/session endpoints take a
// browser session cookie and nothing else today), the policy-endpoint body
// shapes, and the refusal shapes a client has to handle. Kept here, next to
// the generator, so it cannot drift from the inventory it introduces.
const preamble = "## How to read this\n\n" +
	"**auth** is what a request must carry:\n\n" +
	"- `public` — nothing.\n" +
	"- `api_key` — `Authorization: Bearer <key>` with a user-scoped API key (`POST /api/v1/api-keys`). The key's own routing policy applies.\n" +
	"- `api_key|session` — either of the above two forms. These are the OpenAI-compatible routes an SDK points at: `GET /v1/models` and `POST /v1/chat/completions` on the api take a Bearer key exactly like the gateway does, so an SDK never needs a browser session.\n" +
	"- `session` / `admin` — a browser session cookie from `POST /auth/login` (or the OIDC flow). `admin` additionally requires the session's user to have the admin role. **There is no non-interactive credential for the `/api/v1/*` admin surface yet**: an API key does not authenticate these routes, so scripting user, key or policy management means driving the login flow first. This is a known gap, not a design choice. Session requests that change state (POST/PUT/PATCH/DELETE) must also send `X-Requested-With: XMLHttpRequest`; GETs and Bearer requests need not.\n" +
	"- `service` — the gateway's service credential, held by the api; not for customers.\n\n" +
	"**surface** `ui` means a screen in the web app calls the route; `machine` means nothing in the UI does, for the reason given. The parity checks in CI (services/web `pnpm run check:parity`, and `route_inventory_test.go` in each service) hold both directions.\n\n" +
	"### Routing policy bodies\n\n" +
	"Every routing-policy level (organisation, user, API key) reads and writes the same object:\n\n" +
	"```json\n{\n  \"residency\": null | \"self_hosted_only\",\n  \"allowed_models\": null | [\"<model id>\", ...],\n  \"default_model\": null | \"<model id or alias>\"\n}\n```\n\n" +
	"`null` means inherit from the level above (at the organisation, `residency` is `\"any\"` or `\"self_hosted_only\"` — there is nothing above it). A user or key body with all three fields `null` means \"inherit everything\", which is the same as having no row: the `PUT` removes any stored row and answers with no `updated_at`, exactly like `DELETE`; a reconciler that reads the row back should expect that. `allowed_models` takes concrete model ids only, never aliases. The merged result is `.../routing-policy/effective`: `{allowed_models, residency, default_model, sources:{field: \"org\"|\"user\"|\"api_key\"|\"none\"}, reachable_models, stored}`. `POST .../routing-policy/preview` takes the same body as `PUT` and returns `{effective, keys_affected:[{id,name,before,after}], would_lock_out:[...]}` without writing. `PUT /api/v1/routing-policy` (organisation) answers with `affected_principals` (users and keys with a policy of their own that are now allowed fewer models), `org_narrowed` (everyone who inherits is allowed fewer) and, when the new residency is `self_hosted_only`, `unresolved_models` (enabled, allowed models no backend has reported yet — their class is unknown, so they are counted neither as lost nor kept; any that turn out cloud-managed will be refused) when the change tightens.\n\n" +
	"### Refusals\n\n" +
	"- `400 {\"error\": \"...\"}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).\n" +
	"- `409 {\"error\": \"...\", \"locked_out\": [\"<user email or key name>\", ...]}` — the write would leave the named principals (or `\"(organisation)\"`) with no allowed model. Nothing was written.\n" +
	"- `502 {\"error\": \"gateway unavailable: ...\"}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.\n" +
	"- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{\"error\": {\"message\", \"type\", \"code\"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation`, `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered).\n\n" +
	"## Inventory\n\n"

// render builds docs/API.md's full text: one section per service, one line
// per endpoint, in golden order (not re-sorted, so the doc's ordering
// matches the golden file an editor already has open).
func render(entries []routeEntry) string {
	var b strings.Builder
	b.WriteString("# Darkplum API route inventory\n\n")
	b.WriteString("Generated from docs/api-routes.json by cmd/apidoc — do not hand-edit; regenerate with `go run ./cmd/apidoc` from services/api after changing routes.\n\n")
	b.WriteString(preamble)

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
