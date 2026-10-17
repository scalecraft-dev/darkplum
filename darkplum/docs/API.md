# Darkplum API route inventory

Generated from docs/api-routes.json by cmd/apidoc — do not hand-edit; regenerate with `go run ./cmd/apidoc` from services/api after changing routes.

## How to read this

**auth** is what a request must carry:

- `public` — nothing.
- `api_key` — `Authorization: Bearer <key>` with a user-scoped API key (`POST /api/v1/api-keys`). The key's own routing policy applies.
- `api_key|session` — either of the above two forms. These are the OpenAI-compatible routes an SDK points at: `GET /v1/models` and `POST /v1/chat/completions` on the api take a Bearer key exactly like the gateway does, so an SDK never needs a browser session.
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

`null` means inherit from the level above (at the organisation, `residency` is `"any"`, `"cloud_account_only"`, or `"self_hosted_only"` — there is nothing above it). The ladder is tighten-only and ranked `any` < `cloud_account_only` < `self_hosted_only`: `any` permits every backend class, `cloud_account_only` (the default on both fresh and pre-existing installs) permits `self_hosted` and `cloud_managed` but refuses `external_api` (a direct vendor call leaves the cloud account), `self_hosted_only` permits only `self_hosted`. A user or key body with all three fields `null` means "inherit everything", which is the same as having no row: the `PUT` removes any stored row and answers with no `updated_at`, exactly like `DELETE`; a reconciler that reads the row back should expect that. `allowed_models` takes concrete model ids only, never aliases. The merged result is `.../routing-policy/effective`: `{allowed_models, residency, default_model, sources:{field: "org"|"user"|"api_key"|"none"}, reachable_models, stored}`. `POST .../routing-policy/preview` takes the same body as `PUT` and returns `{effective, keys_affected:[{id,name,before,after}], would_lock_out:[...]}` without writing. `PUT /api/v1/routing-policy` (organisation) answers with `affected_principals` (users and keys with a policy of their own that are now allowed fewer models), `org_narrowed` (everyone who inherits is allowed fewer) and, when the new residency tightens to `cloud_account_only` or `self_hosted_only`, `unresolved_models` (enabled, allowed models no backend has reported yet — their class is unknown, so they are counted neither as lost nor kept; any that turn out refused under the new residency will be) when the change tightens.

### Backend class & the capability header

A model's `backend_class` is one of `self_hosted` (inside the customer's cluster), `cloud_managed` (a managed service in the customer's cloud account, e.g. Bedrock), or `external_api` (a direct vendor call — Anthropic, OpenAI — that leaves the cloud account entirely). Requesting `GET /v1/models` (session or Bearer, on both the api and the gateway) and `GET /api/v1/models` (admin) with the request header `X-Darkplum-Classes: external_api` opts into seeing `external_api` models; without it they are omitted from the response regardless of residency — this is a rollout-safety handshake (an old client that never sends it can never be shown, or mislabel, a class it doesn't understand), never an enforcement mechanism, and it has no effect on `POST /v1/chat/completions`. `GET /api/v1/models` (admin) additionally changes response SHAPE on this header: without it, a bare JSON array (unchanged from before vendor models existed); with it, `{"data": [...same rows...], "providers": [{"name": "anthropic"|"openai", "configured": bool, "state": "not_configured"|"ok"|"filter_missing"|"filter_matched_nothing"|"auth_failed"|"unreachable", "model_count": int, "last_sync_at": RFC3339|null, "filter_source": "db"|"env"|"none", "filter": ["<glob or id>", ...], "workspace_id": string|null, "installing_until": RFC3339|null}, ...], "provider_features": ["ui_key_install", ...]}` (`providers` always both vendors, from the gateway's last successful sync; `installing_until` comes from the api's own vendor_provider_settings, null once absent or past — see "Vendor key install" below; `provider_features` is passed through from the gateway's last sync, `[]` before the first one — `ui_key_install` means the gateway serves the vendor-key install routes below). `POST /v1/chat/completions` responses carry `X-Darkplum-Backend-Class` and `X-Darkplum-Provider` (`vllm`|`lmstudio`|`bedrock`|`anthropic`|`openai`|`mock`) alongside the existing `X-Darkplum-Model`/`X-Darkplum-Route`/`X-Darkplum-Router-Reason` headers. Usage-log rows (`GET /api/v1/usage`, its CSV export, and `inference_events` generally) carry `backend_class` (null when a request was refused before any route resolved one) and `provider`; `GET /api/v1/usage`/`/export` accept `?backend_class=` as a comma-separated list of `self_hosted`, `cloud_managed`, `external_api`, `unrecorded` (the last matching a null column).

### Model availability

Each model row (`GET /api/v1/models`, `GET /v1/models`) carries `availability`: `null`, or for Bedrock models the gateway's verbatim object `{"state": "ok"|"needs_aws_opt_in"|"unknown", "reason": "data_retention", "account_mode", "required_mode"}`. `needs_aws_opt_in` means chat will be refused with `model_requires_data_retention` until the AWS account's Bedrock data-retention mode is raised. It is refreshed on every model sync (cleared when the gateway omits it).

### Vendor key install (admin UI)

Four routes let an admin install, rotate, change the catalogued models for, or remove an Anthropic/OpenAI vendor key without a terminal (`make set-<vendor>-key` remains the alternative path; both write the same cluster Secret/ConfigMap, keyed by whichever wrote `filter_set_at` most recently). All four are `admin` session auth ONLY: **a request carrying `Authorization: Bearer` is refused `403 {"error": {"code": "bearer_not_allowed", "message"}}` before anything else runs** (not merely left to fail some other check), because a Bearer credential skips the `X-Requested-With` CSRF check every other admin write requires, and these routes install a credential into the cluster. The vendor key itself is never stored in Postgres, never logged, and never appears in any response body of any of these routes — including error bodies — under any circumstance. Every error response on these four routes, at any status code, is `{"error": {"code": "...", "message": "..."}}` (`models_enabled` additionally carries `models`); `message` is always admin-readable, closed text, never a raw internal error. `POST verify`, `PUT key`, and `PUT filter` are rate-limited (5/min per acting admin, 20/hr across the deployment); exceeding either gets `429` with a `Retry-After` header in whole seconds and `{"error": {"code": "rate_limited", "message"}}`.

- `POST /api/v1/providers/:vendor/verify {key?: string, model_filter?: [string], workspace_id?: string}` — check a key (or, with `key` omitted, the already-installed one) against the vendor through the gateway, without storing anything. `200 {reachable: [ids], admitted: [ids]}` (`admitted` = `reachable` minus the gateway's non-chat hygiene denylist, further narrowed by `model_filter` when non-empty). `422`, code one of `auth_failed`, `unreachable` (a transport failure only: DNS, TLS, timeout, connection refused), `vendor_error` (the vendor ANSWERED with any HTTP status other than 401/403 -- 429 included; the body also carries `vendor_http_status: int` and `vendor_error_type: string` matching `^[a-z_]{1,40}$` or `unknown`, and the message is the closed text "<Vendor> answered HTTP <status> (<type>) to the key check. Nothing was stored."; the vendor's own message is never relayed; `PUT key` and `PUT filter` return the same 422), `key_required` (no key installed and none submitted), `filter_matched_nothing` (a non-empty `model_filter` admitted nothing), `workspace_required` (Anthropic only: the key was created at organization level, not inside a workspace, so Anthropic refuses it without an `anthropic-workspace-id`; the message is the closed text "This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace."; `PUT key` and `PUT filter` return the same 422). `workspace_id` (Anthropic only; `400 invalid_request` for openai, `400 invalid_workspace_id` unless it matches `^[A-Za-z0-9_-]{1,64}$`) is the workspace the check runs against: with a submitted `key` it scopes that key's check; with `key` omitted it overrides the installed workspace id for the check (absent = the installed/effective one, `""` = none).
- `PUT /api/v1/providers/:vendor/key {key: string, model_filter: [string], workspace_id?: string, allow_all?: bool}` — install or rotate. `key` must match `^[A-Za-z0-9_-]{20,512}$`; `model_filter` must be non-empty and (unless `allow_all: true`) contain no bare-wildcard pattern (a pattern of only `*`/`?`, which would admit every model the key can ever reach, including ones not yet released). Verified through the gateway before anything is written; nothing is stored if verification fails. `200 {admitted: [ids], installing_until: RFC3339, warnings?: [string]}` — `installing_until` is ~5 minutes out and is left to expire naturally (never cleared early on success); the admitted-models list and the gateway's own configured state may lag until then. `workspace_id` (Anthropic only, same validation as verify; not a secret) is stored with the filter under the same last-writer-wins stamp and mirrored to the ConfigMap as `ANTHROPIC_WORKSPACE_ID`: **absent or `null` keeps the current value, an explicit `""` clears it**, a value sets it (the same rule on `PUT filter`). `422` (same codes as verify, plus `filter_too_broad`), `409` code `gateway_too_old` (the gateway predates this feature), `503` code one of `not_in_cluster`, `object_missing`, `forbidden`, `unavailable` (plain-language `message`, never the underlying request/response — see the exact wording in the runbook). A mirror-to-ConfigMap or gateway-refresh failure after the key is safely stored is reported in a non-fatal `warnings` array, never as a failed install.
- `PUT /api/v1/providers/:vendor/filter {model_filter: [string], workspace_id?: string, allow_all?: bool}` — change which of an already-installed key's models are catalogued ("Adopt" is this route called with the gateway's own env-reported filter). Verified through the gateway with the INSTALLED key (no `key` field). `200 {admitted: [ids], warnings?: [string]}`, `422` (`key_required`|`vendor_error`|`workspace_required`|`auth_failed`|`unreachable`|`filter_matched_nothing`|`filter_too_broad`), `409 gateway_too_old`, `503` as above. Never sets or clears `installing_until` (a filter change reaches the gateway through the DB snapshot in seconds, not the kubelet-synced Secret path) — it still calls the gateway refresh hint and triggers an immediate resync so the catalogue reflects the change within a few seconds regardless.
- `DELETE /api/v1/providers/:vendor/key {confirm: "<vendor>", disable_models?: bool}` — remove the vendor's key. `confirm` must equal the vendor name (not rate-limited; never calls the vendor). `409` code `models_enabled` plus `models: [ids]` when the vendor has enabled models and `disable_models` isn't set; with it, they're disabled in the same transaction as the removal. `200 {disabled_models?: [ids], warnings?: [string]}`. The stored `model_filter` is kept (so reinstalling recalls the previous selection); `installing_until` is cleared.
- Audit events (`GET /api/v1/activity`): `provider.key_installed` / `provider.key_rotated` (by the gateway's last-known `configured` state for that vendor before the write) on `PUT key`, `provider.filter_changed` on `PUT filter`, `provider.key_removed` on `DELETE key`. `detail` always carries `vendor`, `model_filter_before`, `model_filter_after`, and for Anthropic `workspace_id_before` / `workspace_id_after` (omitted when both are empty); `PUT key`'s event additionally carries `key_last4` (from the submitted body — never read back from storage, which never holds it); `DELETE key`'s event additionally carries `disabled_models` (the model ids turned off in the same transaction) but only when at least one was — it is omitted entirely, not an empty array, otherwise.

### Refusals

- `400 {"error": "..."}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).
- `409 {"error": "...", "locked_out": ["<user email or key name>", ...]}` — the write would leave the named principals (or `"(organisation)"`) with no allowed model. Nothing was written.
- `502 {"error": "gateway unavailable: ..."}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.
- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{"error": {"message", "type", "code"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation` (message names the actual class and the actual residency), `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered), `model_requires_data_retention` (422, the model needs the AWS account's Bedrock data-retention mode raised (Bedrock only); the body also carries `model`, optional `route`, `account_mode`, `required_mode: "aws_review"`), `unsupported_parameter` (400, body names the offending field — `tools`/`tool_choice`/`functions`/`function_call`/a `response_format` of type `json_schema`/non-text content blocks, for every route), `provider_rate_limited` (429, vendor `Retry-After` passed through), `provider_auth_failed` (502, generic text — never echoes the vendor's own error body), `upstream_error` (502), `upstream_timeout` (504).

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
- `GET /api/v1/users` — auth: session, surface: ui
- `GET /api/v1/api-keys` — auth: session, surface: ui
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
- `GET /api/v1/routing-policy/principals` — auth: admin, surface: ui — filters: limit (max 200), offset
- `GET /api/v1/activity` — auth: admin, surface: ui — filters: actor_id, event_type, since, until, limit, offset
- `GET /api/v1/activity/export` — auth: admin, surface: ui — filters: actor_id, event_type, since, until
- `GET /api/v1/usage` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code (a refusal code, or `any`), backend_class (comma-separated list of self_hosted, cloud_managed, external_api, unrecorded), since, until, limit, offset
- `GET /api/v1/usage/export` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code, backend_class, since, until
- `GET /api/v1/usage/stats` — auth: admin, surface: ui — filters: interval, user_id, api_key_id, since, until
- `GET /api/v1/usage/summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/user-summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/key-summary` — auth: admin, surface: ui — filters: since, until
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
- `POST /v1/providers/:vendor/verify` — auth: service, surface: machine (Called only by the api, with the service key, to verify and activate a vendor key an administrator installs from Admin > Models)
- `POST /v1/providers/:vendor/refresh` — auth: service, surface: machine (Called only by the api, with the service key, to verify and activate a vendor key an administrator installs from Admin > Models)

