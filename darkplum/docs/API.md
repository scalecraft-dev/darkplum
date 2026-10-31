# Darkplum API route inventory

Generated from docs/api-routes.json by cmd/apidoc — do not hand-edit; regenerate with `go run ./cmd/apidoc` from services/api after changing routes.

## How to read this

**auth** is what a request must carry:

- `public` — nothing.
- `api_key` — `Authorization: Bearer <key>` with a user-scoped API key (`POST /api/v1/api-keys`). The key's own routing policy applies.
- `api_key|session` — either of the above two forms. These are the OpenAI-compatible routes an SDK points at: `GET /v1/models` and `POST /v1/chat/completions` on the api take a Bearer key exactly like the gateway does, so an SDK never needs a browser session. Bearer chat bodies are forwarded to the gateway unmodified, byte for byte on the api-to-gateway hop (the gateway re-serializes to the vendor): the organisation system prompt applies to session (webchat) chat only.
- `session` / `admin` — a browser session cookie from `POST /auth/login` (or the OIDC flow). `admin` additionally requires the session's user to have the admin role. **There is no non-interactive credential for the `/api/v1/*` admin surface yet**: an API key does not authenticate these routes, so scripting user, key or policy management means driving the login flow first. This is a known gap, not a design choice. Session requests that change state (POST/PUT/PATCH/DELETE) must also send `X-Requested-With: XMLHttpRequest`; GETs and Bearer requests need not.
- `service` — the gateway's service credential, held by the api; not for customers.

**surface** `ui` means a screen in the web app calls the route; `machine` means nothing in the UI does, for the reason given. The parity checks in CI (services/web `pnpm run check:parity`, and `route_inventory_test.go` in each service) hold both directions.

### Routing policy bodies

Every routing-policy level (organisation, user, API key) reads and writes the same object:

```json
{
  "residency": null | "cloud_account_only" | "self_hosted_only",
  "allowed_models": null | ["<model id>", ...],
  "default_model": null | "<model id or alias>"
}
```

`null` means inherit from the level above (at the organisation, `residency` is `"any"`, `"cloud_account_only"`, or `"self_hosted_only"` — there is nothing above it). The ladder is tighten-only and ranked `any` < `cloud_account_only` < `self_hosted_only`: `any` permits every backend class, `cloud_account_only` (the default on both fresh and pre-existing installs) permits `self_hosted` and `cloud_managed` but refuses `external_api` (a direct vendor call leaves the cloud account), `self_hosted_only` permits only `self_hosted`. A user or key body with all three fields `null` means "inherit everything", which is the same as having no row: the `PUT` removes any stored row and answers with no `updated_at`, exactly like `DELETE`; a reconciler that reads the row back should expect that. `allowed_models` takes concrete model ids only, never aliases. The merged result is `.../routing-policy/effective`: `{allowed_models, residency, default_model, sources:{field: "org"|"user"|"api_key"|"none"}, reachable_models, stored}`. `POST .../routing-policy/preview` takes the same body as `PUT` and returns `{effective, keys_affected:[{id,name,before,after}], would_lock_out:[...]}` without writing. `PUT /api/v1/routing-policy` (organisation) answers with `affected_principals` (users and keys with a policy of their own that are now allowed fewer models), `org_narrowed` (everyone who inherits is allowed fewer) and, when the new residency tightens to `cloud_account_only` or `self_hosted_only`, `unresolved_models` (enabled, allowed models no backend has reported yet — their class is unknown, so they are counted neither as lost nor kept; any that turn out refused under the new residency will be) when the change tightens. The organisation body (and only it) also carries `"params_mode": "lenient" | "strict"` (see "Chat parameters" below); `GET` always returns it, `PUT` may omit it to keep the current value, an unknown value is a 400, and a user or key body that sets it is a 400. A change is recorded as `routing_policy.params_mode_changed` with `{previous, new}`.

### Backend class & the capability header

A model's `backend_class` is one of `self_hosted` (inside the customer's cluster), `cloud_managed` (a managed service in the customer's cloud account, e.g. Bedrock), or `external_api` (a direct vendor call — Anthropic, OpenAI — that leaves the cloud account entirely). Requesting `GET /v1/models` (session or Bearer, on both the api and the gateway) and `GET /api/v1/models` (admin) with the request header `X-Darkplum-Classes: external_api` opts into seeing `external_api` models; without it they are omitted from the response regardless of residency — this is a rollout-safety handshake (an old client that never sends it can never be shown, or mislabel, a class it doesn't understand), never an enforcement mechanism, and it has no effect on `POST /v1/chat/completions`. `GET /api/v1/models` (admin) additionally changes response SHAPE on this header: without it, a bare JSON array (unchanged from before vendor models existed); with it, `{"data": [...same rows...], "providers": [{"name": "anthropic"|"openai", "configured": bool, "state": "not_configured"|"ok"|"filter_missing"|"filter_matched_nothing"|"auth_failed"|"unreachable", "model_count": int, "last_sync_at": RFC3339|null, "filter_source": "db"|"env"|"none", "filter": ["<glob or id>", ...], "workspace_id": string|null, "installing_until": RFC3339|null}, ...], "provider_features": ["ui_key_install", ...]}` (`providers` always both vendors, from the gateway's last successful sync; `installing_until` comes from the api's own vendor_provider_settings, null once absent or past — see "Vendor key install" below; `provider_features` is passed through from the gateway's last sync, `[]` before the first one — `ui_key_install` means the gateway serves the vendor-key install routes below). `POST /v1/chat/completions` responses carry `X-Darkplum-Backend-Class` and `X-Darkplum-Provider` (`vllm`|`lmstudio`|`bedrock`|`anthropic`|`openai`|`mock`) alongside the existing `X-Darkplum-Model`/`X-Darkplum-Route`/`X-Darkplum-Router-Reason` headers. Usage-log rows (`GET /api/v1/usage`, its CSV export, and `inference_events` generally) carry `backend_class` (null when a request was refused before any route resolved one) and `provider`; `GET /api/v1/usage`/`/export` accept `?backend_class=` as a comma-separated list of `self_hosted`, `cloud_managed`, `external_api`, `unrecorded` (the last matching a null column). Each model id is also registered (`model_registered_classes`, migration 028) at the strictest backend class a sync has seen serve it. Once the api and gateway both have that migration, and while the registration stands, a request for that id is refused with `model_class_downgraded` (503) instead of being served from a looser class (for example a self-hosted model whose backend is down, answered by a vendor that lists the same id); a routed alias's pick that hits this is treated as the route being down, so `on_route_unavailable` applies. An id served only between two syncs is not registered. The user-key `GET /v1/models` list and the effective-policy reachable-model counts omit such ids; the service-key discovery list does not, so the change stays visible. `PATCH /api/v1/models/:id` with `{"accept_class": true}` re-registers the model at the class it is discovered under now, for a model deliberately moved to a looser class; it is recorded as `model.class_accepted`.

### Model availability

Each model row (`GET /api/v1/models`, `GET /v1/models`) carries `availability`: `null`, or for Bedrock models the gateway's verbatim object `{"state": "ok"|"needs_aws_opt_in"|"unknown", "reason": "data_retention", "account_mode", "required_mode"}`. `needs_aws_opt_in` means chat will be refused with `model_requires_data_retention` until the AWS account's Bedrock data-retention mode is raised. It is refreshed on every model sync (cleared when the gateway omits it).

### Vendor key install (admin UI)

Four routes let an admin install, rotate, change the catalogued models for, or remove an Anthropic/OpenAI vendor key without a terminal (`darkplum keys set <vendor>` remains the alternative path; both write the same cluster Secret/ConfigMap, keyed by whichever wrote `filter_set_at` most recently). All four are `admin` session auth ONLY: **a request carrying `Authorization: Bearer` is refused `403 {"error": {"code": "bearer_not_allowed", "message"}}` before anything else runs** (not merely left to fail some other check), because a Bearer credential skips the `X-Requested-With` CSRF check every other admin write requires, and these routes install a credential into the cluster. The vendor key itself is never stored in Postgres, never logged, and never appears in any response body of any of these routes — including error bodies — under any circumstance. Every error response on these four routes, at any status code, is `{"error": {"code": "...", "message": "..."}}` (`models_enabled` additionally carries `models`); `message` is always admin-readable, closed text, never a raw internal error. `POST verify`, `PUT key`, and `PUT filter` are rate-limited (5/min per acting admin, 20/hr across the deployment); exceeding either gets `429` with a `Retry-After` header in whole seconds and `{"error": {"code": "rate_limited", "message"}}`.

- `POST /api/v1/providers/:vendor/verify {key?: string, model_filter?: [string], workspace_id?: string}` — check a key (or, with `key` omitted, the already-installed one) against the vendor through the gateway, without storing anything. `200 {reachable: [ids], admitted: [ids]}` (`admitted` = `reachable` minus the gateway's non-chat hygiene denylist, further narrowed by `model_filter` when non-empty). `422`, code one of `auth_failed`, `unreachable` (a transport failure only: DNS, TLS, timeout, connection refused), `vendor_error` (the vendor ANSWERED with any HTTP status other than 401/403 -- 429 included; the body also carries `vendor_http_status: int` and `vendor_error_type: string` matching `^[a-z_]{1,40}$` or `unknown`, and the message is the closed text "<Vendor> answered HTTP <status> (<type>) to the key check. Nothing was stored."; the vendor's own message is never relayed; `PUT key` and `PUT filter` return the same 422), `key_required` (no key installed and none submitted), `filter_matched_nothing` (a non-empty `model_filter` admitted nothing), `workspace_required` (Anthropic only: the key was created at organization level, not inside a workspace, so Anthropic refuses it without an `anthropic-workspace-id`; the message is the closed text "This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace."; `PUT key` and `PUT filter` return the same 422). `workspace_id` (Anthropic only; `400 invalid_request` for openai, `400 invalid_workspace_id` unless it matches `^[A-Za-z0-9_-]{1,64}$`) is the workspace the check runs against: with a submitted `key` it scopes that key's check; with `key` omitted it overrides the installed workspace id for the check (absent = the installed/effective one, `""` = none).
- `PUT /api/v1/providers/:vendor/key {key: string, model_filter: [string], workspace_id?: string, allow_all?: bool}` — install or rotate. `key` must match `^[A-Za-z0-9_-]{20,512}$`; `model_filter` must be non-empty and (unless `allow_all: true`) contain no bare-wildcard pattern (a pattern of only `*`/`?`, which would admit every model the key can ever reach, including ones not yet released). Verified through the gateway before anything is written; nothing is stored if verification fails. `200 {admitted: [ids], installing_until: RFC3339, warnings?: [string]}` — `installing_until` is ~5 minutes out and is left to expire naturally (never cleared early on success); the admitted-models list and the gateway's own configured state may lag until then. `workspace_id` (Anthropic only, same validation as verify; not a secret) is stored with the filter under the same last-writer-wins stamp and mirrored to the ConfigMap as `ANTHROPIC_WORKSPACE_ID`: **absent or `null` keeps the current value, an explicit `""` clears it**, a value sets it (the same rule on `PUT filter`). `422` (same codes as verify, plus `filter_too_broad`), `409` code `gateway_too_old` (the gateway predates this feature), `503` code one of `not_in_cluster`, `object_missing`, `forbidden`, `unavailable` (plain-language `message`, never the underlying request/response — see the exact wording in the runbook). A mirror-to-ConfigMap or gateway-refresh failure after the key is safely stored is reported in a non-fatal `warnings` array, never as a failed install.
- `PUT /api/v1/providers/:vendor/filter {model_filter: [string], workspace_id?: string, allow_all?: bool}` — change which of an already-installed key's models are catalogued ("Adopt" is this route called with the gateway's own env-reported filter). Verified through the gateway with the INSTALLED key (no `key` field). `200 {admitted: [ids], warnings?: [string]}`, `422` (`key_required`|`vendor_error`|`workspace_required`|`auth_failed`|`unreachable`|`filter_matched_nothing`|`filter_too_broad`), `409 gateway_too_old`, `503` as above. Never sets or clears `installing_until` (a filter change reaches the gateway through the DB snapshot in seconds, not the kubelet-synced Secret path) — it still calls the gateway refresh hint and triggers an immediate resync so the catalogue reflects the change within a few seconds regardless.
- `DELETE /api/v1/providers/:vendor/key {confirm: "<vendor>", disable_models?: bool}` — remove the vendor's key. `confirm` must equal the vendor name (not rate-limited; never calls the vendor). `409` code `models_enabled` plus `models: [ids]` when the vendor has enabled models and `disable_models` isn't set; with it, they're disabled in the same transaction as the removal. `200 {disabled_models?: [ids], warnings?: [string]}`. The stored `model_filter` is kept (so reinstalling recalls the previous selection); `installing_until` is cleared.
- Audit events (`GET /api/v1/activity`): `provider.key_installed` / `provider.key_rotated` (by the gateway's last-known `configured` state for that vendor before the write) on `PUT key`, `provider.filter_changed` on `PUT filter`, `provider.key_removed` on `DELETE key`. `detail` always carries `vendor`, `model_filter_before`, `model_filter_after`, and for Anthropic `workspace_id_before` / `workspace_id_after` (omitted when both are empty); `PUT key`'s event additionally carries `key_last4` (from the submitted body — never read back from storage, which never holds it); `DELETE key`'s event additionally carries `disabled_models` (the model ids turned off in the same transaction) but only when at least one was — it is omitted entirely, not an empty array, otherwise.

### Spend over time by backend class

`GET /api/v1/usage/spend?group_by=day,backend_class` (or `hour,backend_class` for a 24h view) returns one row per (time bucket, backend class) instead of one per group: `{"key": "<bucket start, RFC3339 UTC>", "label": <same>, "backend_class": "self_hosted|cloud_managed|external_api|unrecorded", requests, prompt_tokens, completion_tokens, cache_read_tokens, cache_creation_tokens, cost_cents, unpriced_requests}`, chronological (bucket, then class). `limit` counts time buckets (the newest N are kept), not rows. Totals, `priced_coverage`, `currency` and `basis` are as for the single-dimension forms, which are unchanged.

### Vendor usage reconciliation (billing keys)

Estimates (price book x recorded tokens) are compared with what the vendor says it billed. Reading the vendor's reports needs a SEPARATE billing key per vendor: an Anthropic Admin API key (`sk-ant-admin01-…`) or an OpenAI organization admin key (`sk-admin-…`); inference keys are refused by those endpoints. Bedrock has no key: its status derives from the api's own AWS credential chain (Cost Explorer `ce:GetCostAndUsage`). The key is stored only in the cluster Secret `darkplum-billing-keys` (file `<vendor>-billing-key`), never in Postgres, logs or responses.

- `POST /api/v1/providers/:vendor/billing-key/verify {key}` — `:vendor` is `anthropic` or `openai` (`404` otherwise). `200 {"state": "ok", "sample": {"day": "YYYY-MM-DD", "buckets": <integer>}}`. `422 {"error": {code, message, vendor_http_status?}}` with `code` one of `auth_failed`, `wrong_key_type`, `unreachable`, `vendor_error` (`message` is closed text, never vendor text). `400 invalid_key_format`, `403 insecure_transport`. Rate-limited like the inference-key routes.
- `PUT /api/v1/providers/:vendor/billing-key {key}` — verify, store in the Secret, audit, then pull in the background. `200 {"state": "installed"}`; `422` as above (nothing stored); `503` `not_in_cluster|object_missing|forbidden|unavailable`. Audit: `provider.billing_key_installed` / `provider.billing_key_rotated` with `{vendor, key_last4}`.
- `DELETE /api/v1/providers/:vendor/billing-key` — no body. `200 {"state": "not_configured"}`; audit `provider.billing_key_removed`. Already-pulled report rows are kept.
- `POST /api/v1/usage/reconciliation/refresh` — pull every configured vendor now. `202 {"state": "pulling"}`; rate-limited to 1/min. The api also pulls at start, daily at 03:00 UTC, and after a key install (last 31 UTC days, both reports).
- `GET /api/v1/usage/reconciliation?since&until&group_by=day|model|vendor` — `{"vendors": [{vendor, configured, state, last_pulled_at, key_last4}], "rows": [{key, label, vendor, estimated_cents, reported_cents, reported_tokens: {input, cached, output} | null, delta_cents}], "currency": "USD"}`. `state` is `not_configured|ok|auth_failed|wrong_key_type|unreachable|pulling`; cents are decimal strings (4 places), `null` when unknown; `delta_cents = reported - estimated` only when both exist. Estimates cover the billable vendors (anthropic, openai, bedrock) for all request purposes. `reported_tokens.input` is uncached + cache-creation tokens and `cached` is cache reads. For `group_by=model`, Anthropic/OpenAI model ids map 1:1; report lines without a model are keyed by their description, and Bedrock Cost Explorer usage types are mapped best-effort (`USE1-ClaudeHaiku45-input-tokens` -> family `claudehaiku45`, prefix-matched against recorded Bedrock model ids; ambiguous or unknown stays keyed by the usage type and labelled `unmapped`). Admin models list `providers[]` gains `billing: {configured, state, last_pulled_at}`.

### Library (skills, context, tools)

`/api/v1/library` holds the items a person keeps in the organisation's own database and can share with colleagues: `kind` is `skill` (instructions, markdown), `context` (reference text, markdown) or `tool` (an MCP server: body is the JSON `{"server_url", "allowed_tools"?}`). An item is `private` (owner only) or `org` (every signed-in user can read and attach it; only the owner edits it). A private item is invisible to everyone else, administrators included: the governance routes under `/api/v1/library/shared` are scoped to `org` items in SQL. Deleting a user removes their private items and reassigns their shared ones to the acting administrator. Every item carries `content_sha256`; a tool item carries `tool: {server_url, catalog, catalog_at, credential: {set, last4}}`. `POST .../tools/verify` connects to the server (host must be on the admin allowlist `PUT /api/v1/settings/tool-hosts {hosts: ["mcp.corp.example", "*.svc.cluster.local"]}`), lists its tools and caches them; `PUT .../tools/credential {token}` stores a bearer token in the cluster Secret `darkplum-tool-credentials` (never Postgres, never returned). Export: one item as `.md` (front matter `name`, `description`, `kind`) or `.json` (tool, no credential); `GET /api/v1/library/export` is a zip of everything the user can see. Import takes `{filename, content, kind?, visibility?}` for one file; `?dry_run=1` previews and lists front-matter keys that would be dropped (only `name`, `description`, `kind` are read; a Claude SKILL.md's scripts and resources are never imported or run).

Attaching to a chat: `POST /v1/chat/completions` (session auth only) takes the header `X-Darkplum-Library-Items: <id>,<id>` (at most 8). Skills and context are injected as a system message after the organisation prompt; tool items become the request's `tools`, and the api runs the tool loop (at most 8 rounds): each call goes to the MCP server, and the stream carries extra `chat.completion.chunk` frames with an empty `choices` array and a `darkplum: {type: "tool_call"|"tool_result", call_id, server, server_name, tool, arguments|content, is_error, duration_ms}` member that OpenAI SDKs ignore and the webchat renders. The response carries `X-Darkplum-Library: <id>:<sha12>,...` and `X-Darkplum-Tools: <count>`. Refusals (OpenAI envelope): `400 library_requires_session` (a Bearer caller sent the header), `400 library_invalid` / `library_too_many` / `library_too_large` (over 100k characters of text) / `library_too_many_tools`, `404 library_item_not_found` (body carries `missing: [ids]`; an item was deleted or unshared), `422 library_tool_unverified` / `library_tool_invalid`, `503 library_unavailable` (the library could not be read: the turn is refused rather than sent without the attachment).

### Chat parameters

Every OpenAI `POST /v1/chat/completions` request parameter has one of five dispositions per backend. The table below is generated from `docs/chat-params.json`, the gateway's own registry (a gateway test fails if the two differ), so it is what the gateway does, not a description of intent.

- **forward**: sent to the backend as is. **translate**: mapped to the backend's equivalent.
- **accept-default**: you sent the backend's default value (`n: 1`, penalties `0`, an empty `logit_bias`, `store: false`, `parallel_tool_calls: true`, `logprobs: false`, `service_tier` `auto`, `response_format` `text`) or `null` for any key. Silent and not recorded.
- **drop-recorded**: not sent. Named in the `X-Darkplum-Dropped-Params` response header and in the usage log, and audited the first time it happens for a key.
- **contract-refuse**: `400` `unsupported_parameter` with `param` naming the field, in every mode, because dropping it would hand you a wrong answer (a JSON-mode request answered in prose, three choices collapsed to one, a forced tool call that was not forced).

**Lenient is the default.** A parameter a backend cannot honour that is not a contract refusal is dropped and recorded rather than failing the request, so an SDK that sends its usual defaults keeps working on every backend. An unknown top-level key (or an unknown key inside `stream_options` or `response_format`) is dropped and recorded under its own name. **Strict mode** is an organisation-wide switch (`params_mode` on `PUT /api/v1/routing-policy`): every drop becomes a `400` `unsupported_parameter` instead, naming the first parameter; contract refusals are unchanged. Dropping is not free: dropping `seed` means the same request can return different text, and dropping `frequency_penalty`, `presence_penalty` or a sampling parameter changes the output.

**Where drops are visible.** (1) `X-Darkplum-Dropped-Params` on the response, decided before the first byte so it is on a stream too: comma-separated, a bare name for a dropped parameter (`seed`, `stream_options.include_obfuscation`), `name=raised` for an adjustment (`max_tokens=raised`: extended thinking needed more room than the limit you set), at most 20 unknown key names and then `unknown_keys=more_N`. Only names are recorded, never the values you sent; but an unknown key's name is chosen by the caller and could itself carry data, so treat it as data in your own account (it never leaves it). (2) The usage log, the same entries: `dropped_params` on each `GET /api/v1/usage` row (an array of strings, `null` when nothing was dropped) and the `dropped_params` column of `GET /api/v1/usage/export` (entries joined with `;`, empty when nothing was dropped); the Activity screen badges such a request. (3) The audit log: one `inference.param_dropped` event the first time each API key (or, for webchat, each user) has a given parameter dropped, never repeated per request.

A parameter is dropped for `reasoning_effort` too where the backend has no equivalent, for a Claude model without extended thinking, and on any turn after a tool call; with extended thinking on, `temperature`, a `top_p` below 0.95 and `top_k` are dropped and `max_tokens` is raised. `user` is dropped (recorded) when the HMAC secret is not installed, and is never sent raw to a vendor; in strict mode that is a `400` (for example on the dev overlay, which installs no secret). A request for audio output (`modalities` other than `["text"]`, or `audio`) or for web search (`web_search_options`) is a contract refusal, not a drop.

**Rollout.** Strict mode only takes effect on gateways running this release. Until migration 029 is applied the gateway reads every organisation as lenient (and logs a warning), and `darkplum deploy` rolls the api and then the gateway in one run, so a strict setting made before the gateway has rolled is ignored by the old gateways. Check `X-Darkplum-Dropped-Params` on a request with an unknown key to confirm the gateway you reach is the new one.

| Parameter | Value | OpenAI | vLLM / LM Studio | Anthropic | Bedrock | Strict mode |
|---|---|---|---|---|---|---|
| `functions` | non-default | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `functions` | default | accept-default | accept-default | accept-default | accept-default | same |
| `function_call` | non-default | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `function_call` | default | accept-default | accept-default | accept-default | accept-default | same |
| `response_format` | json_schema | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `response_format` | unknown-type | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `messages` | non-text-content | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `model` | any | forward | forward | forward | forward | same |
| `messages` | text | forward | forward | translate | translate | same |
| `stream` | any | forward | forward | translate | translate | same |
| `stream_options` | object | forward | forward | forward | forward | same |
| `stream_options.include_usage` | true | forward | forward | translate | translate | same |
| `stream_options.include_usage` | default | accept-default | accept-default | accept-default | accept-default | same |
| `stream_options.include_obfuscation` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `max_tokens` | any | translate | forward | translate | translate | same |
| `max_tokens` * | with-thinking | forward | forward | translate | translate | same |
| `max_completion_tokens` | any | forward | translate | translate | translate | same |
| `max_completion_tokens` | conflicts-with-max_tokens | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `temperature` | any | forward | forward | forward | forward | same |
| `temperature` * | with-thinking | forward | forward | drop-recorded | drop-recorded | refused on Anthropic, Bedrock |
| `top_p` | any | forward | forward | forward | forward | same |
| `top_p` * | with-thinking | forward | forward | drop-recorded | drop-recorded | refused on Anthropic, Bedrock |
| `seed` | any | forward | forward | drop-recorded | drop-recorded | refused on Anthropic, Bedrock |
| `frequency_penalty` | default | accept-default | accept-default | accept-default | accept-default | same |
| `frequency_penalty` | non-default | forward | forward | drop-recorded | drop-recorded | refused on Anthropic, Bedrock |
| `presence_penalty` | default | accept-default | accept-default | accept-default | accept-default | same |
| `presence_penalty` | non-default | forward | forward | drop-recorded | drop-recorded | refused on Anthropic, Bedrock |
| `n` | default | accept-default | accept-default | accept-default | accept-default | same |
| `n` | non-default | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `stop` | any | forward | forward | translate | translate | same |
| `logit_bias` | default | accept-default | accept-default | accept-default | accept-default | same |
| `logit_bias` | non-default | forward | forward | contract-refuse | contract-refuse | same |
| `logprobs` | default | accept-default | accept-default | accept-default | accept-default | same |
| `logprobs` | true | forward | forward | contract-refuse | contract-refuse | same |
| `top_logprobs` | default | accept-default | accept-default | accept-default | accept-default | same |
| `top_logprobs` | non-default | forward | forward | contract-refuse | contract-refuse | same |
| `tools` | default | accept-default | accept-default | accept-default | accept-default | same |
| `tools` | non-empty | forward | forward | translate | translate | same |
| `tool_choice` | auto-or-none | forward | forward | translate | translate | same |
| `tool_choice` | required-or-named | forward | forward | translate | translate | same |
| `parallel_tool_calls` | default | accept-default | accept-default | accept-default | accept-default | same |
| `parallel_tool_calls` | false-with-tools | forward | contract-refuse | translate | contract-refuse | same |
| `parallel_tool_calls` | false-without-tools | accept-default | accept-default | accept-default | accept-default | same |
| `response_format` | text | accept-default | accept-default | accept-default | accept-default | same |
| `response_format` | json_object | forward | forward | contract-refuse | contract-refuse | same |
| `response_format.type` | any | forward | forward | forward | forward | same |
| `response_format.json_schema` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `user` * | any | translate | forward | translate | translate | same |
| `store` | default | forward | accept-default | accept-default | accept-default | same |
| `store` | true | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `reasoning_effort` * | none | forward | accept-default | accept-default | accept-default | same |
| `reasoning_effort` * | other | forward | drop-recorded | translate | translate | refused on vLLM / LM Studio |
| `metadata` | default | accept-default | accept-default | accept-default | accept-default | same |
| `metadata` | non-default | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `service_tier` | default | accept-default | accept-default | accept-default | accept-default | same |
| `service_tier` | non-default | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `modalities` | default | accept-default | accept-default | accept-default | accept-default | same |
| `modalities` | non-default | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `audio` | any | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `prediction` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `web_search_options` | any | contract-refuse | contract-refuse | contract-refuse | contract-refuse | same |
| `verbosity` | default | accept-default | accept-default | accept-default | accept-default | same |
| `verbosity` | non-default | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `safety_identifier` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `prompt_cache_key` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `prompt_cache_retention` | any | drop-recorded | drop-recorded | drop-recorded | drop-recorded | refused on OpenAI, vLLM / LM Studio, Anthropic, Bedrock |
| `top_k` | any | drop-recorded | forward | drop-recorded | drop-recorded | refused on OpenAI, Anthropic, Bedrock |
| `min_p` | any | drop-recorded | forward | drop-recorded | drop-recorded | refused on OpenAI, Anthropic, Bedrock |
| `repetition_penalty` | any | drop-recorded | forward | drop-recorded | drop-recorded | refused on OpenAI, Anthropic, Bedrock |
| `guided_json` | any | drop-recorded | forward | drop-recorded | drop-recorded | refused on OpenAI, Anthropic, Bedrock |
| `chat_template_kwargs` | any | drop-recorded | forward | drop-recorded | drop-recorded | refused on OpenAI, Anthropic, Bedrock |

`*` marks a row the gateway decides per request or per model rather than from the value alone; the table gives the normal outcome. A key the gateway does not know -- at the top level of the body, or inside stream_options or response_format -- is dropped and recorded under its own name; in strict mode it is refused. A null value is silent. The mock backend is a development stand-in that returns canned text and receives no customer traffic. It has no column here and is dispatched with the openaicompat column.

Notes by row:

- `functions` (non-default): Legacy functions API. An empty array or null is the default.
- `function_call` (non-default): Anything other than "none" or null.
- `response_format` (json_schema): Structured outputs.
- `response_format` (unknown-type): A type other than text, json_object or json_schema.
- `messages` (non-text-content): A content array containing anything but text parts.
- `model` (any): An alias resolves to a concrete model id first.
- `messages` (text): Translated to the backend's own message schema where it differs.
- `stream_options` (object): Container: its keys are the rows below. Without stream: true it is a 400 invalid_stream_options.
- `stream_options.include_usage` (true): Anthropic and Bedrock report usage differently; the gateway ends the stream with OpenAI's usage chunk (choices: []).
- `stream_options.include_usage` (default): false.
- `stream_options.include_obfuscation` (any): OpenAI-specific stream padding; the gateway does not forward it.
- `max_tokens` (any): OpenAI receives it as max_completion_tokens.
- `max_tokens` (with-thinking): With reasoning_effort translated to extended thinking the value sent is the caller's plus the thinking budget, capped at the model limit; the change is reported as max_tokens=raised.
- `max_completion_tokens` (any): The same limit as max_tokens under its newer name.
- `max_completion_tokens` (conflicts-with-max_tokens): Both sent and different.
- `temperature` (with-thinking): Extended thinking forbids a caller temperature; it is dropped when reasoning_effort becomes thinking.
- `top_p` (with-thinking): A top_p below 0.95 is dropped when reasoning_effort becomes extended thinking.
- `seed` (any): Dropping it removes reproducibility: the same request can return different text. Anthropic and Bedrock have no seed.
- `frequency_penalty` (default): 0.
- `frequency_penalty` (non-default): Dropping it changes the output: the model repeats itself more.
- `presence_penalty` (default): 0.
- `presence_penalty` (non-default): Dropping it changes the output.
- `n` (default): 1.
- `n` (non-default): Anything but 1.
- `stop` (any): Anthropic stop_sequences, Bedrock stopSequences.
- `logit_bias` (default): Empty.
- `logprobs` (default): false.
- `top_logprobs` (default): 0.
- `top_logprobs` (non-default): Requires logprobs: true, which a backend that cannot return logprobs refuses first.
- `tools` (default): Empty array.
- `tools` (non-empty): Translated to Anthropic tools and Bedrock toolConfig.
- `tool_choice` (required-or-named): A refusal is the answer for a backend that cannot enforce it; every current backend can.
- `parallel_tool_calls` (default): true.
- `parallel_tool_calls` (false-with-tools): Anthropic receives disable_parallel_tool_use. vLLM and Bedrock cannot enforce it.
- `parallel_tool_calls` (false-without-tools): Nothing to restrict without tools.
- `response_format` (text): The default.
- `response_format.type` (any): Selects the response_format row above.
- `response_format.json_schema` (any): Only meaningful with type json_schema, which is refused before this is reached.
- `user` (any): In-cluster vLLM receives the value as sent. OpenAI (user), Anthropic (metadata.user_id) and Bedrock (requestMetadata) receive a keyed HMAC, never the raw value. If the HMAC secret is not installed it is dropped and recorded.
- `store` (default): false, or null. OpenAI receives store: false.
- `store` (true): true.
- `reasoning_effort` (none): No reasoning requested; nothing is sent or recorded except to OpenAI, where none is a real setting.
- `reasoning_effort` (other): OpenAI forwards it. Anthropic and Anthropic models on Bedrock translate it to extended thinking (low and minimal 1024, medium 4096, high and xhigh 16384 budget tokens); it is dropped and recorded for a model without extended thinking and on any turn after a tool call, and refused with a required or named tool_choice. Other backends drop it.
- `metadata` (default): Empty object.
- `service_tier` (default): auto or default.
- `modalities` (default): ["text"].
- `modalities` (non-default): Anything but ["text"], i.e. a request for audio output.
- `web_search_options` (any): A request for web search is refused rather than answered without it.
- `verbosity` (default): medium.
- `top_k` (any): Anthropic has a top_k of its own but the gateway does not translate it.
- `chat_template_kwargs` (any): Only enable_thinking, thinking and reasoning_effort, scalar values.

### Refusals

- `400 {"error": "..."}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).
- `409 {"error": "...", "locked_out": ["<user email or key name>", ...]}` — the write would leave the named principals (or `"(organisation)"`) with no allowed model. Nothing was written.
- `502 {"error": "gateway unavailable: ..."}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.
- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{"error": {"message", "type", "code"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation` (message names the actual class and the actual residency), `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered), `model_class_downgraded` (503, `type: service_unavailable`, the model id is registered at a stricter backend class than the one now serving it and the request was not sent there; see "Backend class & the capability header"), `route_target_unavailable` (503, `type: service_unavailable`, the classifier's picked non-default route of a routed alias targets a disabled or unreachable model and the alias's `on_route_unavailable` setting (`refuse`, `fallback_same_or_stricter` — the default —, `fallback_any_permitted`) does not allow the default route to answer: `refuse` never does, `fallback_same_or_stricter` only when the default route's model is usable and its backend class is the same or stricter than the picked route's, unknown classes refusing; the message names both routes, both classes and the setting; the request is never sent anywhere else), `model_requires_data_retention` (422, the model needs the AWS account's Bedrock data-retention mode raised (Bedrock only); the body also carries `model`, optional `route`, `account_mode`, `required_mode: "aws_review"`), `unsupported_parameter` (400, `param` names the offending field, for a contract refusal in every mode (and, in strict mode only, for any parameter that would otherwise be dropped; see "Chat parameters"); a parameter whose silent loss would give a wrong answer is refused rather than dropped. On every route: `functions`/`function_call`/a `response_format` of type `json_schema`/non-text content blocks, and `max_tokens` and `max_completion_tokens` sent with different values (`param` is `max_completion_tokens`). Once the model is resolved, for the backend serving it: `tools` when it cannot take tools; `n` greater than 1 (every backend returns one choice); `store: true` (on every backend; completions are never stored at a vendor on your behalf, so send `store: false` or omit it); `response_format` of type `json_object` on Anthropic and Bedrock (OpenAI and vLLM take it); `logprobs: true` and a non-empty `logit_bias` on Anthropic and Bedrock (OpenAI and vLLM apply them, and `logprobs` come back in OpenAI's shape on `choices[].logprobs`, streamed and not); `stop` where unsupported; `tool_choice` `required` or a named function where the backend cannot enforce it; `parallel_tool_calls: false` when tools are sent, on vLLM and Bedrock (OpenAI enforces it; Anthropic receives it as `disable_parallel_tool_use`). The defaults are accepted everywhere: `n: 1`, `store: false`, `logprobs: false`, an empty `logit_bias`, `parallel_tool_calls: true`, zero penalties, `response_format` of type `text`, and `null` for any of them. A malformed value for a parameter the gateway forwards (`top_logprobs` outside 0-20 or without `logprobs: true`, a `logit_bias` value outside -100 to 100 or a key that is not an integer token id, a penalty outside -2 to 2, `min_p` outside 0-1, `repetition_penalty` not above 0, `chat_template_kwargs` that is not an allowed object) is a 400 `invalid_request` with `param` naming it. These parameters are forwarded where the backend has an equivalent: `seed`, `frequency_penalty` and `presence_penalty` on OpenAI and vLLM (Anthropic and Bedrock have none: dropped and recorded); `user` raw only to in-cluster vLLM and LM Studio, and to OpenAI (`user`), Anthropic (`metadata.user_id`) and Bedrock (`requestMetadata`) only as `k1:` plus the hex HMAC-SHA256 of the value under a per-installation secret, so the identifier you send never leaves your cluster (the three vendors receive the same value for the same person, so one person can be correlated across vendors; if the secret is not installed the field is omitted and recorded as dropped, never sent raw); and the vLLM extensions `top_k`, `min_p`, `repetition_penalty`, `guided_json` and `chat_template_kwargs` on self-hosted vLLM only. `logit_bias` takes at most 300 integer token-id keys; `guided_json` is capped at 64 KiB; `chat_template_kwargs` is capped at 4 KiB and accepts only `enable_thinking`, `thinking` and `reasoning_effort` (scalar values), because any other template variable could rewrite the prompt after policy checks ran. `max_completion_tokens` is accepted as `max_tokens` on every backend. `stream_options` without `stream: true` is a 400 `invalid_stream_options` (`param: stream_options`, recorded as `invalid_request`); with `stream_options.include_usage: true` a stream ends with OpenAI's separate usage chunk (`choices: []`, `usage` set) after the `finish_reason` chunk, and without it usage stays on the `finish_reason` chunk), `invalid_tools` (400, a malformed `tools` array or `tool_choice`, or an unpaired tool turn; `param` names the field; recorded on the usage-log row as `invalid_request`), `provider_rate_limited` (429, vendor `Retry-After` passed through), `provider_auth_failed` (502, generic text — never echoes the vendor's own error body), `upstream_error` (502), `upstream_timeout` (504). Refusals produced before the gateway sees the request use the same envelope: `not_authenticated` (401, no session and no Bearer), `invalid_api_key` (401, the gateway refused the Bearer key — also on `GET /v1/models`, which never falls back to an unfiltered list), `invalid_body` (400, session callers only: not a JSON object), `invalid_messages` (400, session callers only: `messages` empty or a turn without a known `role`; Bearer callers get the gateway's own refusal instead: `invalid_messages` for an empty or role-less `messages`, `model_required` when no model is given, a code-less `invalid_request` 400 for an unparseable body), `request_too_large` (413, body over 16 MiB at any hop), `unknown_endpoint` (404, any other `/v1/*` path or method), `gateway_unavailable` (502/503).
- `reasoning_effort` (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`; any other value is a 400 `invalid_request`) is handled per backend and, on Anthropic, per model. OpenAI: forwarded as sent. Anthropic, and Anthropic models served by Bedrock: translated to extended thinking with a `budget_tokens` of 1024 (`minimal`, `low`), 4096 (`medium`) or 16384 (`high`, `xhigh`), for models that support it; `none` leaves thinking off. Support is decided per model: Anthropic reports it per model through model discovery, so until discovery has run (right after a restart) `reasoning_effort` is dropped; on Bedrock it is matched by model id (Claude 3.7 Sonnet and the Claude 4 generation onward, including inference-profile ids and foundation-model ARNs), so an application inference profile ARN, whose id does not name the model, is treated as unsupported. Where it is dropped the request is still served, without it, and the drop is recorded. `budget_tokens` is a target, not a reservation: the gateway sends `max_tokens` plus the budget as the vendor `max_tokens` (capped at the model's output limit; a `max_tokens` of yours that is already at the limit is not lowered), so a response may use up to that many output tokens and you are billed for what it uses. `usage.completion_tokens` includes the thinking tokens, so it can exceed the visible answer. The model's reasoning is never returned and never stored. With thinking on, `temperature`, a `top_p` below 0.95 and `top_k` are not sent (the vendor rejects them alongside thinking; recorded as dropped), and a `tool_choice` of `required` or a named function is refused with a 400 `unsupported_parameter` naming `reasoning_effort`; `auto` and `none` work. A request whose `messages` already contain an assistant tool call or a tool result runs with thinking off (recorded as dropped), because Anthropic requires its signed thinking blocks to be sent back and an OpenAI client never has them; this is checked before the forced `tool_choice` refusal, so such a request is served. On a model or backend with no equivalent (Anthropic and Bedrock models without extended thinking, vLLM) the parameter is accepted and not sent (recorded as dropped).

## Inventory

## api

- `GET /health` — auth: public, surface: machine (Kubernetes liveness/readiness probe and load-balancer health check; no browser surface)
- `GET /auth/config` — auth: public, surface: ui
- `POST /auth/login` — auth: public, surface: ui
- `POST /auth/change-password` — auth: session, surface: ui
- `POST /auth/setup` — auth: public, surface: ui
- `GET /auth/me` — auth: session, surface: ui
- `GET /auth/login` — auth: public, surface: ui
- `GET /auth/callback` — auth: public, surface: machine (OIDC provider redirect target reached by browser navigation, never by an API call)
- `POST /auth/logout` — auth: session, surface: ui
- `GET /api/v1/users` — auth: admin, surface: ui
- `GET /api/v1/api-keys` — auth: admin, surface: ui
- `GET /api/v1/conversations` — auth: session, surface: ui — filters: limit
- `POST /api/v1/conversations` — auth: session, surface: ui
- `GET /api/v1/conversations/:id` — auth: session, surface: ui — filters: format=export
- `PATCH /api/v1/conversations/:id` — auth: session, surface: ui
- `POST /api/v1/conversations/:id/messages` — auth: session, surface: ui
- `DELETE /api/v1/conversations/:id` — auth: session, surface: ui
- `DELETE /api/v1/conversations/:id/messages` — auth: session, surface: ui
- `GET /api/v1/me/routing-policy/effective` — auth: session, surface: ui
- `POST /api/v1/users` — auth: admin, surface: ui
- `DELETE /api/v1/users/:id` — auth: admin, surface: ui
- `PATCH /api/v1/users/:id/role` — auth: admin, surface: ui
- `PATCH /api/v1/users/:id/password` — auth: admin, surface: ui
- `GET /api/v1/users/:id/routing-policy` — auth: admin, surface: machine (Stored row only, for scripts and GitOps-style reconcilers; the UI reads the same row through .../routing-policy/effective's `stored` field alongside the merged result, so the editor never calls this one)
- `PUT /api/v1/users/:id/routing-policy` — auth: admin, surface: ui
- `DELETE /api/v1/users/:id/routing-policy` — auth: admin, surface: ui
- `GET /api/v1/users/:id/routing-policy/effective` — auth: admin, surface: ui
- `POST /api/v1/users/:id/routing-policy/preview` — auth: admin, surface: ui
- `POST /api/v1/api-keys` — auth: admin, surface: ui
- `DELETE /api/v1/api-keys/:id` — auth: admin, surface: ui
- `GET /api/v1/api-keys/:id/routing-policy` — auth: admin, surface: machine (Stored row only, for scripts and GitOps-style reconcilers; the UI reads the same row through .../routing-policy/effective's `stored` field alongside the merged result, so the editor never calls this one)
- `PUT /api/v1/api-keys/:id/routing-policy` — auth: admin, surface: ui
- `DELETE /api/v1/api-keys/:id/routing-policy` — auth: admin, surface: ui
- `GET /api/v1/api-keys/:id/routing-policy/effective` — auth: admin, surface: ui
- `POST /api/v1/api-keys/:id/routing-policy/preview` — auth: admin, surface: ui
- `GET /api/v1/models` — auth: admin, surface: ui
- `PATCH /api/v1/models/:id` — auth: admin, surface: ui
- `POST /api/v1/providers/:vendor/verify` — auth: admin, surface: ui
- `PUT /api/v1/providers/:vendor/key` — auth: admin, surface: ui
- `PUT /api/v1/providers/:vendor/filter` — auth: admin, surface: ui
- `DELETE /api/v1/providers/:vendor/key` — auth: admin, surface: ui
- `GET /api/v1/model-aliases` — auth: admin, surface: ui
- `PUT /api/v1/model-aliases/:alias` — auth: admin, surface: ui
- `DELETE /api/v1/model-aliases/:alias` — auth: admin, surface: ui
- `POST /api/v1/model-aliases/:alias/test` — auth: admin, surface: ui
- `GET /api/v1/routing-policy` — auth: admin, surface: ui
- `PUT /api/v1/routing-policy` — auth: admin, surface: ui
- `GET /api/v1/settings/system-prompt` — auth: admin, surface: ui
- `PUT /api/v1/settings/system-prompt` — auth: admin, surface: ui
- `GET /api/v1/library` — auth: session, surface: ui — filters: kind (skill, context, tool), scope (mine, shared, all; default all), q (name/description search, max 100 characters)
- `POST /api/v1/library` — auth: session, surface: ui
- `GET /api/v1/library/export` — auth: session, surface: ui
- `POST /api/v1/library/import` — auth: session, surface: ui — filters: dry_run=1 (parse and preview only; nothing is written)
- `GET /api/v1/library/:id` — auth: session, surface: ui
- `PUT /api/v1/library/:id` — auth: session, surface: ui
- `DELETE /api/v1/library/:id` — auth: session, surface: ui
- `POST /api/v1/library/:id/share` — auth: session, surface: ui
- `GET /api/v1/library/:id/export` — auth: session, surface: ui
- `POST /api/v1/library/:id/tools/verify` — auth: session, surface: ui
- `PUT /api/v1/library/:id/tools/credential` — auth: session, surface: ui
- `DELETE /api/v1/library/:id/tools/credential` — auth: session, surface: ui
- `GET /api/v1/library/shared` — auth: admin, surface: ui
- `POST /api/v1/library/shared/:id/unshare` — auth: admin, surface: ui
- `DELETE /api/v1/library/shared/:id` — auth: admin, surface: ui
- `GET /api/v1/settings/tool-hosts` — auth: admin, surface: ui
- `PUT /api/v1/settings/tool-hosts` — auth: admin, surface: ui
- `GET /api/v1/routing-policy/principals` — auth: admin, surface: ui — filters: limit (max 200), offset
- `GET /api/v1/activity` — auth: admin, surface: ui — filters: actor_id, event_type, since, until, limit, offset
- `GET /api/v1/activity/export` — auth: admin, surface: ui — filters: actor_id, event_type, since, until
- `GET /api/v1/usage` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code (a refusal code, or `any`), backend_class (comma-separated list of self_hosted, cloud_managed, external_api, unrecorded), since, until, limit, offset
- `GET /api/v1/usage/export` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code, backend_class, since, until
- `GET /api/v1/usage/stats` — auth: admin, surface: ui — filters: interval, user_id, api_key_id, since, until
- `GET /api/v1/usage/summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/user-summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/key-summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/spend` — auth: admin, surface: ui — filters: group_by (required: user, api_key, model, provider, backend_class, alias, route, day, or the two-dimensional `day,backend_class` / `hour,backend_class`), user_id, api_key_id, model, purpose, denial_code, backend_class, since, until, limit (default 50, max 500; for the two-dimensional forms: number of time buckets, default 31 days / 24 hours, max 744)
- `POST /api/v1/providers/:vendor/billing-key/verify` — auth: admin, surface: ui
- `PUT /api/v1/providers/:vendor/billing-key` — auth: admin, surface: ui
- `DELETE /api/v1/providers/:vendor/billing-key` — auth: admin, surface: ui
- `GET /api/v1/usage/reconciliation` — auth: admin, surface: ui — filters: group_by (day, model, vendor; default day), since, until (RFC3339; default the last 30 days)
- `POST /api/v1/usage/reconciliation/refresh` — auth: admin, surface: ui
- `GET /api/v1/model-prices` — auth: admin, surface: ui
- `PUT /api/v1/model-prices` — auth: admin, surface: ui
- `DELETE /api/v1/model-prices/:id` — auth: admin, surface: ui
- `GET /v1/models` — auth: api_key|session, surface: ui
- `POST /v1/chat/completions` — auth: api_key|session, surface: ui

## gateway

- `GET /health` — auth: public, surface: machine (Kubernetes probe and operator diagnostics (routing, policy, classifier state); the api's /health is what the browser reaches)
- `GET /v1/models` — auth: api_key, surface: machine (Discovered-model list for the api's catalog sync (service key) and for SDK callers with a user key; the browser uses the api's /v1/models)
- `POST /v1/chat/completions` — auth: api_key, surface: machine (Inference endpoint; the browser reaches it through the api proxy (/v1/chat/completions on the api), SDKs call it with a user key)
- `POST /v1/routing/dryrun` — auth: api_key, surface: machine (Called by the api on behalf of the admin Test-a-prompt panel (POST /api/v1/model-aliases/:alias/test) with the service key)
- `GET /v1/policy/effective` — auth: service, surface: machine (Single effective-policy implementation, consulted by the api for every effective/preview/me view with the service key)
- `POST /v1/policy/effective` — auth: service, surface: machine (Preview counterpart of GET /v1/policy/effective, consulted by the api's routing-policy impact-preview endpoints (POST .../users/:id/routing-policy/preview, .../api-keys/:id/routing-policy/preview) so preview and real save share the one gateway merge implementation)
- `POST /v1/policy/impact` — auth: service, surface: machine (Org-wide lock-out check consulted by the api's PUT /api/v1/routing-policy before it writes: re-merges every principal with an explicit policy row under the proposed org policy in one call, so the org PUT refuses (409) a lock-out exactly like the user/key PUTs do, with the one gateway merge implementation)
- `POST /v1/policy/refresh` — auth: service, surface: machine (Reload the policy snapshot now. The api calls it after every policy-shaped write (model enable/disable, alias, org and per-principal policy) so chat enforcement, effective-policy reads and lock-out previews reflect the write immediately instead of up to POLICY_REFRESH_SECONDS later)
- `POST /v1/providers/:vendor/verify` — auth: service, surface: machine (Called only by the api, with the service key, to verify and activate a vendor key an administrator installs from Admin > Models)
- `POST /v1/providers/:vendor/refresh` — auth: service, surface: machine (Called only by the api, with the service key, to verify and activate a vendor key an administrator installs from Admin > Models)

