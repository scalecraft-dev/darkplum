/**
 * Every HTTP endpoint this app talks to, in one place.
 *
 * Two rules keep this honest:
 *   1. No literal request path exists anywhere else in `src`. `api.ts` builds
 *      every URL from an entry here; `AuthContext.tsx` is the one other caller
 *      allowed to hold a `fetch`, and its paths are entries here too.
 *   2. `scripts/check-api-parity.mts` diffs this table against the server's
 *      golden route list (`docs/api-routes.json`) in both directions, so an
 *      endpoint the UI calls but the server does not serve — or one the server
 *      marks `surface: "ui"` but nothing here calls — fails CI.
 *
 * Paths are templates in the server's own notation (`:id`), not interpolated
 * strings, so the diff is a plain string comparison.
 */

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export interface Route {
  readonly method: HttpMethod
  readonly path: string
}

export const ROUTES = {
  // --- Session / auth (served by the api, outside /api/v1) ---
  authConfig:     { method: 'GET',    path: '/auth/config' },
  authMe:         { method: 'GET',    path: '/auth/me' },
  authLogin:      { method: 'POST',   path: '/auth/login' },
  /** The OIDC hand-off. A browser navigation, not a fetch, but it is still a
   *  route the UI reaches for and so it belongs in the table. */
  authLoginRedirect: { method: 'GET', path: '/auth/login' },
  authLogout:     { method: 'POST',   path: '/auth/logout' },
  authSetup:      { method: 'POST',   path: '/auth/setup' },
  authChangePassword: { method: 'POST', path: '/auth/change-password' },

  // --- Users ---
  listUsers:         { method: 'GET',    path: '/api/v1/users' },
  createUser:        { method: 'POST',   path: '/api/v1/users' },
  deleteUser:        { method: 'DELETE', path: '/api/v1/users/:id' },
  resetUserPassword: { method: 'PATCH',  path: '/api/v1/users/:id/password' },
  updateUserRole:    { method: 'PATCH',  path: '/api/v1/users/:id/role' },

  // --- API keys ---
  listApiKeys:  { method: 'GET',    path: '/api/v1/api-keys' },
  createApiKey: { method: 'POST',   path: '/api/v1/api-keys' },
  revokeApiKey: { method: 'DELETE', path: '/api/v1/api-keys/:id' },

  // --- Activity & usage ---
  listActivity:   { method: 'GET', path: '/api/v1/activity' },
  exportActivity: { method: 'GET', path: '/api/v1/activity/export' },
  listUsage:      { method: 'GET', path: '/api/v1/usage' },
  exportUsage:    { method: 'GET', path: '/api/v1/usage/export' },
  usageStats:     { method: 'GET', path: '/api/v1/usage/stats' },
  usageSummary:   { method: 'GET', path: '/api/v1/usage/summary' },
  usageUserSummary: { method: 'GET', path: '/api/v1/usage/user-summary' },
  usageKeySummary:  { method: 'GET', path: '/api/v1/usage/key-summary' },

  // --- Models ---
  listChatModels:  { method: 'GET',   path: '/v1/models' },
  chatCompletions: { method: 'POST',  path: '/v1/chat/completions' },
  listAdminModels: { method: 'GET',   path: '/api/v1/models' },
  updateModel:     { method: 'PATCH', path: '/api/v1/models/:id' },

  // --- Model aliases ---
  listModelAliases: { method: 'GET',    path: '/api/v1/model-aliases' },
  putModelAlias:    { method: 'PUT',    path: '/api/v1/model-aliases/:alias' },
  testModelAlias:   { method: 'POST',   path: '/api/v1/model-aliases/:alias/test' },
  deleteModelAlias: { method: 'DELETE', path: '/api/v1/model-aliases/:alias' },

  // --- Routing policy: organisation ---
  getRoutingPolicy:    { method: 'GET', path: '/api/v1/routing-policy' },
  updateRoutingPolicy: { method: 'PUT', path: '/api/v1/routing-policy' },
  listPolicyPrincipals: { method: 'GET', path: '/api/v1/routing-policy/principals' },

  // --- Routing policy: per user ---
  // The stored row is read through .../effective's `stored` field, which is
  // what the editor needs (own row and merged result together); the plain
  // GET .../routing-policy is a machine route for scripts (see the golden).
  putUserPolicy:       { method: 'PUT',    path: '/api/v1/users/:id/routing-policy' },
  deleteUserPolicy:    { method: 'DELETE', path: '/api/v1/users/:id/routing-policy' },
  userPolicyEffective: { method: 'GET',    path: '/api/v1/users/:id/routing-policy/effective' },
  userPolicyPreview:   { method: 'POST',   path: '/api/v1/users/:id/routing-policy/preview' },

  // --- Routing policy: per API key ---
  putKeyPolicy:       { method: 'PUT',    path: '/api/v1/api-keys/:id/routing-policy' },
  deleteKeyPolicy:    { method: 'DELETE', path: '/api/v1/api-keys/:id/routing-policy' },
  keyPolicyEffective: { method: 'GET',    path: '/api/v1/api-keys/:id/routing-policy/effective' },
  keyPolicyPreview:   { method: 'POST',   path: '/api/v1/api-keys/:id/routing-policy/preview' },

  // --- Routing policy: the signed-in person's own ---
  myPolicyEffective: { method: 'GET', path: '/api/v1/me/routing-policy/effective' },

  // --- Conversations ---
  listConversations:     { method: 'GET',    path: '/api/v1/conversations' },
  createConversation:    { method: 'POST',   path: '/api/v1/conversations' },
  getConversation:       { method: 'GET',    path: '/api/v1/conversations/:id' },
  updateConversation:    { method: 'PATCH',  path: '/api/v1/conversations/:id' },
  deleteConversation:    { method: 'DELETE', path: '/api/v1/conversations/:id' },
  appendMessages:        { method: 'POST',   path: '/api/v1/conversations/:id/messages' },
  truncateMessages:      { method: 'DELETE', path: '/api/v1/conversations/:id/messages' },
} as const

export type RouteName = keyof typeof ROUTES

/** Every route as a flat list — what the parity script diffs. Typing it as
 *  `Route[]` is also what checks each entry above has a valid method. */
export const ALL_ROUTES: readonly Route[] = Object.values(ROUTES)

export type QueryValue = string | number | boolean | undefined | null

/**
 * Turns a route template into a request URL. Path parameters are percent-encoded
 * (alias names reach `:alias` straight from an administrator's keyboard), and
 * query values that are undefined or null are dropped rather than sent empty.
 */
export function buildPath(
  route: Route,
  params?: Record<string, string>,
  query?: Record<string, QueryValue>,
): string {
  let path = route.path
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      path = path.replace(`:${key}`, encodeURIComponent(value))
    }
  }
  if (!query) return path
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const qs = search.toString()
  return qs ? `${path}?${qs}` : path
}
