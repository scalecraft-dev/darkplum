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
	"api GET /api/v1/usage/spend":               "group_by (required: user, api_key, model, provider, backend_class, alias, route, day, or the two-dimensional `day,backend_class` / `hour,backend_class`), user_id, api_key_id, model, purpose, denial_code, backend_class, since, until, limit (default 50, max 500; for the two-dimensional forms: number of time buckets, default 31 days / 24 hours, max 744)",
	"api GET /api/v1/usage/reconciliation":      "group_by (day, model, vendor; default day), since, until (RFC3339; default the last 30 days)",
	"api GET /api/v1/conversations":             "limit",
	"api GET /api/v1/conversations/:id":         "format=export",
	"api GET /api/v1/routing-policy/principals": "limit (max 200), offset",
	"api GET /api/v1/library":                   "kind (skill, context, tool), scope (mine, shared, all; default all), q (name/description search, max 100 characters)",
	"api POST /api/v1/library/import":           "dry_run=1 (parse and preview only; nothing is written)",
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
	"A model's `backend_class` is one of `self_hosted` (inside the customer's cluster), `cloud_managed` (a managed service in the customer's cloud account, e.g. Bedrock), or `external_api` (a direct vendor call — Anthropic, OpenAI — that leaves the cloud account entirely). Requesting `GET /v1/models` (session or Bearer, on both the api and the gateway) and `GET /api/v1/models` (admin) with the request header `X-Darkplum-Classes: external_api` opts into seeing `external_api` models; without it they are omitted from the response regardless of residency — this is a rollout-safety handshake (an old client that never sends it can never be shown, or mislabel, a class it doesn't understand), never an enforcement mechanism, and it has no effect on `POST /v1/chat/completions`. `GET /api/v1/models` (admin) additionally changes response SHAPE on this header: without it, a bare JSON array (unchanged from before vendor models existed); with it, `{\"data\": [...same rows...], \"providers\": [{\"name\": \"anthropic\"|\"openai\", \"configured\": bool, \"state\": \"not_configured\"|\"ok\"|\"filter_missing\"|\"filter_matched_nothing\"|\"auth_failed\"|\"unreachable\", \"model_count\": int, \"last_sync_at\": RFC3339|null, \"filter_source\": \"db\"|\"env\"|\"none\", \"filter\": [\"<glob or id>\", ...], \"workspace_id\": string|null, \"installing_until\": RFC3339|null}, ...], \"provider_features\": [\"ui_key_install\", ...]}` (`providers` always both vendors, from the gateway's last successful sync; `installing_until` comes from the api's own vendor_provider_settings, null once absent or past — see \"Vendor key install\" below; `provider_features` is passed through from the gateway's last sync, `[]` before the first one — `ui_key_install` means the gateway serves the vendor-key install routes below). `POST /v1/chat/completions` responses carry `X-Darkplum-Backend-Class` and `X-Darkplum-Provider` (`vllm`|`lmstudio`|`bedrock`|`anthropic`|`openai`|`mock`) alongside the existing `X-Darkplum-Model`/`X-Darkplum-Route`/`X-Darkplum-Router-Reason` headers. Usage-log rows (`GET /api/v1/usage`, its CSV export, and `inference_events` generally) carry `backend_class` (null when a request was refused before any route resolved one) and `provider`; `GET /api/v1/usage`/`/export` accept `?backend_class=` as a comma-separated list of `self_hosted`, `cloud_managed`, `external_api`, `unrecorded` (the last matching a null column). Each model id is also registered (`model_registered_classes`, migration 028) at the strictest backend class a sync has seen serve it. Once the api and gateway both have that migration, and while the registration stands, a request for that id is refused with `model_class_downgraded` (503) instead of being served from a looser class (for example a self-hosted model whose backend is down, answered by a vendor that lists the same id); a routed alias's pick that hits this is treated as the route being down, so `on_route_unavailable` applies. An id served only between two syncs is not registered. The user-key `GET /v1/models` list and the effective-policy reachable-model counts omit such ids; the service-key discovery list does not, so the change stays visible. `PATCH /api/v1/models/:id` with `{\"accept_class\": true}` re-registers the model at the class it is discovered under now, for a model deliberately moved to a looser class; it is recorded as `model.class_accepted`.\n\n" +
	"### Model availability\n\n" +
	"Each model row (`GET /api/v1/models`, `GET /v1/models`) carries `availability`: `null`, or for Bedrock models the gateway's verbatim object `{\"state\": \"ok\"|\"needs_aws_opt_in\"|\"unknown\", \"reason\": \"data_retention\", \"account_mode\", \"required_mode\"}`. `needs_aws_opt_in` means chat will be refused with `model_requires_data_retention` until the AWS account's Bedrock data-retention mode is raised. It is refreshed on every model sync (cleared when the gateway omits it).\n\n" +
	"### Vendor key install (admin UI)\n\n" +
	"Four routes let an admin install, rotate, change the catalogued models for, or remove an Anthropic/OpenAI vendor key without a terminal (`make set-<vendor>-key` remains the alternative path; both write the same cluster Secret/ConfigMap, keyed by whichever wrote `filter_set_at` most recently). All four are `admin` session auth ONLY: **a request carrying `Authorization: Bearer` is refused `403 {\"error\": {\"code\": \"bearer_not_allowed\", \"message\"}}` before anything else runs** (not merely left to fail some other check), because a Bearer credential skips the `X-Requested-With` CSRF check every other admin write requires, and these routes install a credential into the cluster. The vendor key itself is never stored in Postgres, never logged, and never appears in any response body of any of these routes — including error bodies — under any circumstance. Every error response on these four routes, at any status code, is `{\"error\": {\"code\": \"...\", \"message\": \"...\"}}` (`models_enabled` additionally carries `models`); `message` is always admin-readable, closed text, never a raw internal error. `POST verify`, `PUT key`, and `PUT filter` are rate-limited (5/min per acting admin, 20/hr across the deployment); exceeding either gets `429` with a `Retry-After` header in whole seconds and `{\"error\": {\"code\": \"rate_limited\", \"message\"}}`.\n\n" +
	"- `POST /api/v1/providers/:vendor/verify {key?: string, model_filter?: [string], workspace_id?: string}` — check a key (or, with `key` omitted, the already-installed one) against the vendor through the gateway, without storing anything. `200 {reachable: [ids], admitted: [ids]}` (`admitted` = `reachable` minus the gateway's non-chat hygiene denylist, further narrowed by `model_filter` when non-empty). `422`, code one of `auth_failed`, `unreachable` (a transport failure only: DNS, TLS, timeout, connection refused), `vendor_error` (the vendor ANSWERED with any HTTP status other than 401/403 -- 429 included; the body also carries `vendor_http_status: int` and `vendor_error_type: string` matching `^[a-z_]{1,40}$` or `unknown`, and the message is the closed text \"<Vendor> answered HTTP <status> (<type>) to the key check. Nothing was stored.\"; the vendor's own message is never relayed; `PUT key` and `PUT filter` return the same 422), `key_required` (no key installed and none submitted), `filter_matched_nothing` (a non-empty `model_filter` admitted nothing), `workspace_required` (Anthropic only: the key was created at organization level, not inside a workspace, so Anthropic refuses it without an `anthropic-workspace-id`; the message is the closed text \"This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace.\"; `PUT key` and `PUT filter` return the same 422). `workspace_id` (Anthropic only; `400 invalid_request` for openai, `400 invalid_workspace_id` unless it matches `^[A-Za-z0-9_-]{1,64}$`) is the workspace the check runs against: with a submitted `key` it scopes that key's check; with `key` omitted it overrides the installed workspace id for the check (absent = the installed/effective one, `\"\"` = none).\n" +
	"- `PUT /api/v1/providers/:vendor/key {key: string, model_filter: [string], workspace_id?: string, allow_all?: bool}` — install or rotate. `key` must match `^[A-Za-z0-9_-]{20,512}$`; `model_filter` must be non-empty and (unless `allow_all: true`) contain no bare-wildcard pattern (a pattern of only `*`/`?`, which would admit every model the key can ever reach, including ones not yet released). Verified through the gateway before anything is written; nothing is stored if verification fails. `200 {admitted: [ids], installing_until: RFC3339, warnings?: [string]}` — `installing_until` is ~5 minutes out and is left to expire naturally (never cleared early on success); the admitted-models list and the gateway's own configured state may lag until then. `workspace_id` (Anthropic only, same validation as verify; not a secret) is stored with the filter under the same last-writer-wins stamp and mirrored to the ConfigMap as `ANTHROPIC_WORKSPACE_ID`: **absent or `null` keeps the current value, an explicit `\"\"` clears it**, a value sets it (the same rule on `PUT filter`). `422` (same codes as verify, plus `filter_too_broad`), `409` code `gateway_too_old` (the gateway predates this feature), `503` code one of `not_in_cluster`, `object_missing`, `forbidden`, `unavailable` (plain-language `message`, never the underlying request/response — see the exact wording in the runbook). A mirror-to-ConfigMap or gateway-refresh failure after the key is safely stored is reported in a non-fatal `warnings` array, never as a failed install.\n" +
	"- `PUT /api/v1/providers/:vendor/filter {model_filter: [string], workspace_id?: string, allow_all?: bool}` — change which of an already-installed key's models are catalogued (\"Adopt\" is this route called with the gateway's own env-reported filter). Verified through the gateway with the INSTALLED key (no `key` field). `200 {admitted: [ids], warnings?: [string]}`, `422` (`key_required`|`vendor_error`|`workspace_required`|`auth_failed`|`unreachable`|`filter_matched_nothing`|`filter_too_broad`), `409 gateway_too_old`, `503` as above. Never sets or clears `installing_until` (a filter change reaches the gateway through the DB snapshot in seconds, not the kubelet-synced Secret path) — it still calls the gateway refresh hint and triggers an immediate resync so the catalogue reflects the change within a few seconds regardless.\n" +
	"- `DELETE /api/v1/providers/:vendor/key {confirm: \"<vendor>\", disable_models?: bool}` — remove the vendor's key. `confirm` must equal the vendor name (not rate-limited; never calls the vendor). `409` code `models_enabled` plus `models: [ids]` when the vendor has enabled models and `disable_models` isn't set; with it, they're disabled in the same transaction as the removal. `200 {disabled_models?: [ids], warnings?: [string]}`. The stored `model_filter` is kept (so reinstalling recalls the previous selection); `installing_until` is cleared.\n" +
	"- Audit events (`GET /api/v1/activity`): `provider.key_installed` / `provider.key_rotated` (by the gateway's last-known `configured` state for that vendor before the write) on `PUT key`, `provider.filter_changed` on `PUT filter`, `provider.key_removed` on `DELETE key`. `detail` always carries `vendor`, `model_filter_before`, `model_filter_after`, and for Anthropic `workspace_id_before` / `workspace_id_after` (omitted when both are empty); `PUT key`'s event additionally carries `key_last4` (from the submitted body — never read back from storage, which never holds it); `DELETE key`'s event additionally carries `disabled_models` (the model ids turned off in the same transaction) but only when at least one was — it is omitted entirely, not an empty array, otherwise.\n\n" +
	"### Spend over time by backend class\n\n" +
	"`GET /api/v1/usage/spend?group_by=day,backend_class` (or `hour,backend_class` for a 24h view) returns one row per (time bucket, backend class) instead of one per group: `{\"key\": \"<bucket start, RFC3339 UTC>\", \"label\": <same>, \"backend_class\": \"self_hosted|cloud_managed|external_api|unrecorded\", requests, prompt_tokens, completion_tokens, cache_read_tokens, cache_creation_tokens, cost_cents, unpriced_requests}`, chronological (bucket, then class). `limit` counts time buckets (the newest N are kept), not rows. Totals, `priced_coverage`, `currency` and `basis` are as for the single-dimension forms, which are unchanged.\n\n" +
	"### Vendor usage reconciliation (billing keys)\n\n" +
	"Estimates (price book x recorded tokens) are compared with what the vendor says it billed. Reading the vendor's reports needs a SEPARATE billing key per vendor: an Anthropic Admin API key (`sk-ant-admin01-…`) or an OpenAI organization admin key (`sk-admin-…`); inference keys are refused by those endpoints. Bedrock has no key: its status derives from the api's own AWS credential chain (Cost Explorer `ce:GetCostAndUsage`). The key is stored only in the cluster Secret `darkplum-billing-keys` (file `<vendor>-billing-key`), never in Postgres, logs or responses.\n\n" +
	"- `POST /api/v1/providers/:vendor/billing-key/verify {key}` — `:vendor` is `anthropic` or `openai` (`404` otherwise). `200 {\"state\": \"ok\", \"sample\": {\"day\": \"YYYY-MM-DD\", \"buckets\": <integer>}}`. `422 {\"error\": {code, message, vendor_http_status?}}` with `code` one of `auth_failed`, `wrong_key_type`, `unreachable`, `vendor_error` (`message` is closed text, never vendor text). `400 invalid_key_format`, `403 insecure_transport`. Rate-limited like the inference-key routes.\n" +
	"- `PUT /api/v1/providers/:vendor/billing-key {key}` — verify, store in the Secret, audit, then pull in the background. `200 {\"state\": \"installed\"}`; `422` as above (nothing stored); `503` `not_in_cluster|object_missing|forbidden|unavailable`. Audit: `provider.billing_key_installed` / `provider.billing_key_rotated` with `{vendor, key_last4}`.\n" +
	"- `DELETE /api/v1/providers/:vendor/billing-key` — no body. `200 {\"state\": \"not_configured\"}`; audit `provider.billing_key_removed`. Already-pulled report rows are kept.\n" +
	"- `POST /api/v1/usage/reconciliation/refresh` — pull every configured vendor now. `202 {\"state\": \"pulling\"}`; rate-limited to 1/min. The api also pulls at start, daily at 03:00 UTC, and after a key install (last 31 UTC days, both reports).\n" +
	"- `GET /api/v1/usage/reconciliation?since&until&group_by=day|model|vendor` — `{\"vendors\": [{vendor, configured, state, last_pulled_at, key_last4}], \"rows\": [{key, label, vendor, estimated_cents, reported_cents, reported_tokens: {input, cached, output} | null, delta_cents}], \"currency\": \"USD\"}`. `state` is `not_configured|ok|auth_failed|wrong_key_type|unreachable|pulling`; cents are decimal strings (4 places), `null` when unknown; `delta_cents = reported - estimated` only when both exist. Estimates cover the billable vendors (anthropic, openai, bedrock) for all request purposes. `reported_tokens.input` is uncached + cache-creation tokens and `cached` is cache reads. For `group_by=model`, Anthropic/OpenAI model ids map 1:1; report lines without a model are keyed by their description, and Bedrock Cost Explorer usage types are mapped best-effort (`USE1-ClaudeHaiku45-input-tokens` -> family `claudehaiku45`, prefix-matched against recorded Bedrock model ids; ambiguous or unknown stays keyed by the usage type and labelled `unmapped`). Admin models list `providers[]` gains `billing: {configured, state, last_pulled_at}`.\n\n" +
	"### Library (skills, context, tools)\n\n" +
	"`/api/v1/library` holds the items a person keeps in the organisation's own database and can share with colleagues: `kind` is `skill` (instructions, markdown), `context` (reference text, markdown) or `tool` (an MCP server: body is the JSON `{\"server_url\", \"allowed_tools\"?}`). An item is `private` (owner only) or `org` (every signed-in user can read and attach it; only the owner edits it). A private item is invisible to everyone else, administrators included: the governance routes under `/api/v1/library/shared` are scoped to `org` items in SQL. Deleting a user removes their private items and reassigns their shared ones to the acting administrator. Every item carries `content_sha256`; a tool item carries `tool: {server_url, catalog, catalog_at, credential: {set, last4}}`. `POST .../tools/verify` connects to the server (host must be on the admin allowlist `PUT /api/v1/settings/tool-hosts {hosts: [\"mcp.corp.example\", \"*.svc.cluster.local\"]}`), lists its tools and caches them; `PUT .../tools/credential {token}` stores a bearer token in the cluster Secret `darkplum-tool-credentials` (never Postgres, never returned). Export: one item as `.md` (front matter `name`, `description`, `kind`) or `.json` (tool, no credential); `GET /api/v1/library/export` is a zip of everything the user can see. Import takes `{filename, content, kind?, visibility?}` for one file; `?dry_run=1` previews and lists front-matter keys that would be dropped (only `name`, `description`, `kind` are read; a Claude SKILL.md's scripts and resources are never imported or run).\n\n" +
	"Attaching to a chat: `POST /v1/chat/completions` (session auth only) takes the header `X-Darkplum-Library-Items: <id>,<id>` (at most 8). Skills and context are injected as a system message after the organisation prompt; tool items become the request's `tools`, and the api runs the tool loop (at most 8 rounds): each call goes to the MCP server, and the stream carries extra `chat.completion.chunk` frames with an empty `choices` array and a `darkplum: {type: \"tool_call\"|\"tool_result\", call_id, server, server_name, tool, arguments|content, is_error, duration_ms}` member that OpenAI SDKs ignore and the webchat renders. The response carries `X-Darkplum-Library: <id>:<sha12>,...` and `X-Darkplum-Tools: <count>`. Refusals (OpenAI envelope): `400 library_requires_session` (a Bearer caller sent the header), `400 library_invalid` / `library_too_many` / `library_too_large` (over 100k characters of text) / `library_too_many_tools`, `404 library_item_not_found` (body carries `missing: [ids]`; an item was deleted or unshared), `422 library_tool_unverified` / `library_tool_invalid`, `503 library_unavailable` (the library could not be read: the turn is refused rather than sent without the attachment).\n\n" +
	"### Refusals\n\n" +
	"- `400 {\"error\": \"...\"}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).\n" +
	"- `409 {\"error\": \"...\", \"locked_out\": [\"<user email or key name>\", ...]}` — the write would leave the named principals (or `\"(organisation)\"`) with no allowed model. Nothing was written.\n" +
	"- `502 {\"error\": \"gateway unavailable: ...\"}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.\n" +
	"- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{\"error\": {\"message\", \"type\", \"code\"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation` (message names the actual class and the actual residency), `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered), `model_class_downgraded` (503, `type: service_unavailable`, the model id is registered at a stricter backend class than the one now serving it and the request was not sent there; see \"Backend class & the capability header\"), `route_target_unavailable` (503, `type: service_unavailable`, the classifier's picked non-default route of a routed alias targets a disabled or unreachable model and the alias's `on_route_unavailable` setting (`refuse`, `fallback_same_or_stricter` — the default —, `fallback_any_permitted`) does not allow the default route to answer: `refuse` never does, `fallback_same_or_stricter` only when the default route's model is usable and its backend class is the same or stricter than the picked route's, unknown classes refusing; the message names both routes, both classes and the setting; the request is never sent anywhere else), `model_requires_data_retention` (422, the model needs the AWS account's Bedrock data-retention mode raised (Bedrock only); the body also carries `model`, optional `route`, `account_mode`, `required_mode: \"aws_review\"`), `unsupported_parameter` (400, body names the offending field — `tools`/`tool_choice`/`functions`/`function_call`/a `response_format` of type `json_schema`/non-text content blocks, for every route), `provider_rate_limited` (429, vendor `Retry-After` passed through), `provider_auth_failed` (502, generic text — never echoes the vendor's own error body), `upstream_error` (502), `upstream_timeout` (504). Refusals produced before the gateway sees the request use the same envelope: `not_authenticated` (401, no session and no Bearer), `invalid_api_key` (401, the gateway refused the Bearer key — also on `GET /v1/models`, which never falls back to an unfiltered list), `invalid_body` (400, not a JSON object), `invalid_messages` (400, `messages` empty or a turn without a known `role`), `request_too_large` (413, body over 16 MiB at any hop), `unknown_endpoint` (404, any other `/v1/*` path or method), `gateway_unavailable` (502/503).\n\n" +
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
