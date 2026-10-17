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
	"api GET /api/v1/usage":                     "user_id, api_key_id, model, purpose, denial_code (a refusal code, or `any`), backend_class (comma-separated list of self_hosted, cloud_managed, external_api, unrecorded), since, until, limit, offset",
	"api GET /api/v1/usage/export":              "user_id, api_key_id, model, purpose, denial_code, backend_class, since, until",
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
	"```json\n{\n  \"residency\": null | \"cloud_account_only\" | \"self_hosted_only\",\n  \"allowed_models\": null | [\"<model id>\", ...],\n  \"default_model\": null | \"<model id or alias>\"\n}\n```\n\n" +
	"`null` means inherit from the level above (at the organisation, `residency` is `\"any\"`, `\"cloud_account_only\"`, or `\"self_hosted_only\"` — there is nothing above it). The ladder is tighten-only and ranked `any` < `cloud_account_only` < `self_hosted_only`: `any` permits every backend class, `cloud_account_only` (the default on both fresh and pre-existing installs) permits `self_hosted` and `cloud_managed` but refuses `external_api` (a direct vendor call leaves the cloud account), `self_hosted_only` permits only `self_hosted`. A user or key body with all three fields `null` means \"inherit everything\", which is the same as having no row: the `PUT` removes any stored row and answers with no `updated_at`, exactly like `DELETE`; a reconciler that reads the row back should expect that. `allowed_models` takes concrete model ids only, never aliases. The merged result is `.../routing-policy/effective`: `{allowed_models, residency, default_model, sources:{field: \"org\"|\"user\"|\"api_key\"|\"none\"}, reachable_models, stored}`. `POST .../routing-policy/preview` takes the same body as `PUT` and returns `{effective, keys_affected:[{id,name,before,after}], would_lock_out:[...]}` without writing. `PUT /api/v1/routing-policy` (organisation) answers with `affected_principals` (users and keys with a policy of their own that are now allowed fewer models), `org_narrowed` (everyone who inherits is allowed fewer) and, when the new residency tightens to `cloud_account_only` or `self_hosted_only`, `unresolved_models` (enabled, allowed models no backend has reported yet — their class is unknown, so they are counted neither as lost nor kept; any that turn out refused under the new residency will be) when the change tightens.\n\n" +
	"### Backend class & the capability header\n\n" +
	"A model's `backend_class` is one of `self_hosted` (inside the customer's cluster), `cloud_managed` (a managed service in the customer's cloud account, e.g. Bedrock), or `external_api` (a direct vendor call — Anthropic, OpenAI — that leaves the cloud account entirely). Requesting `GET /v1/models` (session or Bearer, on both the api and the gateway) and `GET /api/v1/models` (admin) with the request header `X-Darkplum-Classes: external_api` opts into seeing `external_api` models; without it they are omitted from the response regardless of residency — this is a rollout-safety handshake (an old client that never sends it can never be shown, or mislabel, a class it doesn't understand), never an enforcement mechanism, and it has no effect on `POST /v1/chat/completions`. `GET /api/v1/models` (admin) additionally changes response SHAPE on this header: without it, a bare JSON array (unchanged from before vendor models existed); with it, `{\"data\": [...same rows...], \"providers\": [{\"name\": \"anthropic\"|\"openai\", \"configured\": bool, \"state\": \"not_configured\"|\"ok\"|\"filter_missing\"|\"filter_matched_nothing\"|\"auth_failed\"|\"unreachable\", \"model_count\": int, \"last_sync_at\": RFC3339|null, \"filter_source\": \"db\"|\"env\"|\"none\", \"filter\": [\"<glob or id>\", ...], \"installing_until\": RFC3339|null}, ...], \"provider_features\": [\"ui_key_install\", ...]}` (`providers` always both vendors, from the gateway's last successful sync; `installing_until` comes from the api's own vendor_provider_settings, null once absent or past — see \"Vendor key install\" below; `provider_features` is passed through from the gateway's last sync, `[]` before the first one — `ui_key_install` means the gateway serves the vendor-key install routes below). `POST /v1/chat/completions` responses carry `X-Darkplum-Backend-Class` and `X-Darkplum-Provider` (`vllm`|`lmstudio`|`bedrock`|`anthropic`|`openai`|`mock`) alongside the existing `X-Darkplum-Model`/`X-Darkplum-Route`/`X-Darkplum-Router-Reason` headers. Usage-log rows (`GET /api/v1/usage`, its CSV export, and `inference_events` generally) carry `backend_class` (null when a request was refused before any route resolved one) and `provider`; `GET /api/v1/usage`/`/export` accept `?backend_class=` as a comma-separated list of `self_hosted`, `cloud_managed`, `external_api`, `unrecorded` (the last matching a null column).\n\n" +
	"### Vendor key install (admin UI)\n\n" +
	"Four routes let an admin install, rotate, change the catalogued models for, or remove an Anthropic/OpenAI vendor key without a terminal (`make set-<vendor>-key` remains the alternative path; both write the same cluster Secret/ConfigMap, keyed by whichever wrote `filter_set_at` most recently). All four are `admin` session auth ONLY: **a request carrying `Authorization: Bearer` is refused `403 {\"error\": {\"code\": \"bearer_not_allowed\", \"message\"}}` before anything else runs** (not merely left to fail some other check), because a Bearer credential skips the `X-Requested-With` CSRF check every other admin write requires, and these routes install a credential into the cluster. The vendor key itself is never stored in Postgres, never logged, and never appears in any response body of any of these routes — including error bodies — under any circumstance. Every error response on these four routes, at any status code, is `{\"error\": {\"code\": \"...\", \"message\": \"...\"}}` (`models_enabled` additionally carries `models`); `message` is always admin-readable, closed text, never a raw internal error. `POST verify`, `PUT key`, and `PUT filter` are rate-limited (5/min per acting admin, 20/hr across the deployment); exceeding either gets `429` with a `Retry-After` header in whole seconds and `{\"error\": {\"code\": \"rate_limited\", \"message\"}}`.\n\n" +
	"- `POST /api/v1/providers/:vendor/verify {key?: string, model_filter?: [string]}` — check a key (or, with `key` omitted, the already-installed one) against the vendor through the gateway, without storing anything. `200 {reachable: [ids], admitted: [ids]}` (`admitted` = `reachable` minus the gateway's non-chat hygiene denylist, further narrowed by `model_filter` when non-empty). `422`, code one of `auth_failed`, `unreachable`, `key_required` (no key installed and none submitted), `filter_matched_nothing` (a non-empty `model_filter` admitted nothing).\n" +
	"- `PUT /api/v1/providers/:vendor/key {key: string, model_filter: [string], allow_all?: bool}` — install or rotate. `key` must match `^[A-Za-z0-9_-]{20,512}$`; `model_filter` must be non-empty and (unless `allow_all: true`) contain no bare-wildcard pattern (a pattern of only `*`/`?`, which would admit every model the key can ever reach, including ones not yet released). Verified through the gateway before anything is written; nothing is stored if verification fails. `200 {admitted: [ids], installing_until: RFC3339, warnings?: [string]}` — `installing_until` is ~5 minutes out and is left to expire naturally (never cleared early on success); the admitted-models list and the gateway's own configured state may lag until then. `422` (same codes as verify, plus `filter_too_broad`), `409` code `gateway_too_old` (the gateway predates this feature), `503` code one of `not_in_cluster`, `object_missing`, `forbidden`, `unavailable` (plain-language `message`, never the underlying request/response — see the exact wording in the runbook). A mirror-to-ConfigMap or gateway-refresh failure after the key is safely stored is reported in a non-fatal `warnings` array, never as a failed install.\n" +
	"- `PUT /api/v1/providers/:vendor/filter {model_filter: [string], allow_all?: bool}` — change which of an already-installed key's models are catalogued (\"Adopt\" is this route called with the gateway's own env-reported filter). Verified through the gateway with the INSTALLED key (no `key` field). `200 {admitted: [ids], warnings?: [string]}`, `422` (`key_required`|`filter_matched_nothing`|`filter_too_broad`), `409 gateway_too_old`, `503` as above. Never sets or clears `installing_until` (a filter change reaches the gateway through the DB snapshot in seconds, not the kubelet-synced Secret path) — it still calls the gateway refresh hint and triggers an immediate resync so the catalogue reflects the change within a few seconds regardless.\n" +
	"- `DELETE /api/v1/providers/:vendor/key {confirm: \"<vendor>\", disable_models?: bool}` — remove the vendor's key. `confirm` must equal the vendor name (not rate-limited; never calls the vendor). `409` code `models_enabled` plus `models: [ids]` when the vendor has enabled models and `disable_models` isn't set; with it, they're disabled in the same transaction as the removal. `200 {disabled_models?: [ids], warnings?: [string]}`. The stored `model_filter` is kept (so reinstalling recalls the previous selection); `installing_until` is cleared.\n" +
	"- Audit events (`GET /api/v1/activity`): `provider.key_installed` / `provider.key_rotated` (by the gateway's last-known `configured` state for that vendor before the write) on `PUT key`, `provider.filter_changed` on `PUT filter`, `provider.key_removed` on `DELETE key`. `detail` always carries `vendor`, `model_filter_before`, `model_filter_after`; `PUT key`'s event additionally carries `key_last4` (from the submitted body — never read back from storage, which never holds it); `DELETE key`'s event additionally carries `disabled_models` (the model ids turned off in the same transaction) but only when at least one was — it is omitted entirely, not an empty array, otherwise.\n\n" +
	"### Refusals\n\n" +
	"- `400 {\"error\": \"...\"}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).\n" +
	"- `409 {\"error\": \"...\", \"locked_out\": [\"<user email or key name>\", ...]}` — the write would leave the named principals (or `\"(organisation)\"`) with no allowed model. Nothing was written.\n" +
	"- `502 {\"error\": \"gateway unavailable: ...\"}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.\n" +
	"- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{\"error\": {\"message\", \"type\", \"code\"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation` (message names the actual class and the actual residency), `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered), `unsupported_parameter` (400, body names the offending field — `tools`/`tool_choice`/`functions`/`function_call`/a `response_format` of type `json_schema`/non-text content blocks, for every route), `provider_rate_limited` (429, vendor `Retry-After` passed through), `provider_auth_failed` (502, generic text — never echoes the vendor's own error body), `upstream_error` (502), `upstream_timeout` (504).\n\n" +
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
