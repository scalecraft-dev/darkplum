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
  "residency": null | "self_hosted_only",
  "allowed_models": null | ["<model id>", ...],
  "default_model": null | "<model id or alias>"
}
```

`null` means inherit from the level above (at the organisation, `residency` is `"any"` or `"self_hosted_only"` — there is nothing above it). A user or key body with all three fields `null` means "inherit everything", which is the same as having no row: the `PUT` removes any stored row and answers with no `updated_at`, exactly like `DELETE`; a reconciler that reads the row back should expect that. `allowed_models` takes concrete model ids only, never aliases. The merged result is `.../routing-policy/effective`: `{allowed_models, residency, default_model, sources:{field: "org"|"user"|"api_key"|"none"}, reachable_models, stored}`. `POST .../routing-policy/preview` takes the same body as `PUT` and returns `{effective, keys_affected:[{id,name,before,after}], would_lock_out:[...]}` without writing. `PUT /api/v1/routing-policy` (organisation) answers with `affected_principals` (users and keys with a policy of their own that are now allowed fewer models), `org_narrowed` (everyone who inherits is allowed fewer) and, when the new residency is `self_hosted_only`, `unresolved_models` (enabled, allowed models no backend has reported yet — their class is unknown, so they are counted neither as lost nor kept; any that turn out cloud-managed will be refused) when the change tightens.

### Refusals

- `400 {"error": "..."}` — a body that does not validate (unknown model id, an alias in `allowed_models`, a `default_model` outside the effective allowlist).
- `409 {"error": "...", "locked_out": ["<user email or key name>", ...]}` — the write would leave the named principals (or `"(organisation)"`) with no allowed model. Nothing was written.
- `502 {"error": "gateway unavailable: ..."}` — a write or preview that needs the gateway's merge could not reach it. Nothing was written.
- On `POST /v1/chat/completions`, a refusal is an OpenAI-style error `{"error": {"message", "type", "code"}}`. `code` is recorded on the usage-log row as `denial_code` and is one of: `model_not_found` (404), `model_not_enabled`, `model_residency_violation`, `model_not_allowed` (403, `type: permission_error`), `policy_unavailable` (503, the gateway has no policy snapshot and fails closed), `invalid_request` (400, unparseable body or no `model` and no default), `request_canceled` (the caller disconnected before a model answered).

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
- `GET /api/v1/model-aliases` — auth: admin, surface: ui
- `PUT /api/v1/model-aliases/:alias` — auth: admin, surface: ui
- `DELETE /api/v1/model-aliases/:alias` — auth: admin, surface: ui
- `POST /api/v1/model-aliases/:alias/test` — auth: admin, surface: ui
- `GET /api/v1/routing-policy` — auth: admin, surface: ui
- `PUT /api/v1/routing-policy` — auth: admin, surface: ui
- `GET /api/v1/routing-policy/principals` — auth: admin, surface: ui — filters: limit (max 200), offset
- `GET /api/v1/activity` — auth: admin, surface: ui — filters: actor_id, event_type, since, until, limit, offset
- `GET /api/v1/activity/export` — auth: admin, surface: ui — filters: actor_id, event_type, since, until
- `GET /api/v1/usage` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code (a refusal code, or `any`), since, until, limit, offset
- `GET /api/v1/usage/export` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, denial_code, since, until
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

