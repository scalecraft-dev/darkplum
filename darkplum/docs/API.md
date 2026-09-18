# Darkplum API route inventory

Generated from docs/api-routes.json by cmd/apidoc — do not hand-edit; regenerate with `go run ./cmd/apidoc` from services/api after changing routes.

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
- `GET /api/v1/users/:id/routing-policy` — auth: admin, surface: ui
- `PUT /api/v1/users/:id/routing-policy` — auth: admin, surface: ui
- `DELETE /api/v1/users/:id/routing-policy` — auth: admin, surface: ui
- `GET /api/v1/users/:id/routing-policy/effective` — auth: admin, surface: ui
- `POST /api/v1/users/:id/routing-policy/preview` — auth: admin, surface: ui
- `POST /api/v1/api-keys` — auth: admin, surface: ui
- `DELETE /api/v1/api-keys/:id` — auth: admin, surface: ui
- `GET /api/v1/api-keys/:id/routing-policy` — auth: admin, surface: ui
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
- `GET /api/v1/usage` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, since, until, limit, offset
- `GET /api/v1/usage/export` — auth: admin, surface: ui — filters: user_id, api_key_id, model, purpose, since, until
- `GET /api/v1/usage/stats` — auth: admin, surface: ui — filters: interval, user_id, api_key_id, since, until
- `GET /api/v1/usage/summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/user-summary` — auth: admin, surface: ui — filters: since, until
- `GET /api/v1/usage/key-summary` — auth: admin, surface: ui — filters: since, until
- `GET /v1/models` — auth: session, surface: ui
- `POST /v1/chat/completions` — auth: session, surface: ui

## gateway

- `GET /health` — auth: public, surface: machine (Kubernetes probe and operator diagnostics (routing, policy, classifier state); the api's /health is what the browser reaches)
- `GET /v1/models` — auth: api_key, surface: machine (Discovered-model list for the api's catalog sync (service key) and for SDK callers with a user key; the browser uses the api's /v1/models)
- `POST /v1/chat/completions` — auth: api_key, surface: machine (Inference endpoint; the browser reaches it through the api proxy (/v1/chat/completions on the api), SDKs call it with a user key)
- `POST /v1/routing/dryrun` — auth: api_key, surface: machine (Called by the api on behalf of the admin Test-a-prompt panel (POST /api/v1/model-aliases/:alias/test) with the service key)
- `GET /v1/policy/effective` — auth: service, surface: machine (Single effective-policy implementation, consulted by the api for every effective/preview/me view with the service key)
- `POST /v1/policy/effective` — auth: service, surface: machine (Preview counterpart of GET /v1/policy/effective, consulted by the api's routing-policy impact-preview endpoints (POST .../users/:id/routing-policy/preview, .../api-keys/:id/routing-policy/preview) so preview and real save share the one gateway merge implementation)
- `POST /v1/policy/impact` — auth: service, surface: machine (Org-wide lock-out check consulted by the api's PUT /api/v1/routing-policy before it writes: re-merges every principal with an explicit policy row under the proposed org policy in one call, so the org PUT refuses (409) a lock-out exactly like the user/key PUTs do, with the one gateway merge implementation)

