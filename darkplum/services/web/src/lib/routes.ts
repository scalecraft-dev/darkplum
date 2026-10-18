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
  /** Estimated cost at list price, grouped one way (group_by). */
  usageSpend:       { method: 'GET', path: '/api/v1/usage/spend' },
  /** Darkplum's estimate beside each vendor's own reported usage and cost. */
  usageReconciliation:        { method: 'GET',  path: '/api/v1/usage/reconciliation' },
  refreshUsageReconciliation: { method: 'POST', path: '/api/v1/usage/reconciliation/refresh' },

  // --- Model prices (the list-price table spend is estimated from) ---
  listModelPrices:  { method: 'GET',    path: '/api/v1/model-prices' },
  putModelPrice:    { method: 'PUT',    path: '/api/v1/model-prices' },
  deleteModelPrice: { method: 'DELETE', path: '/api/v1/model-prices/:id' },

  // --- Models ---
  listChatModels:  { method: 'GET',   path: '/v1/models' },
  chatCompletions: { method: 'POST',  path: '/v1/chat/completions' },
  listAdminModels: { method: 'GET',   path: '/api/v1/models' },
  updateModel:     { method: 'PATCH', path: '/api/v1/models/:id' },

  // --- Vendor provider keys (admin session only) ---
  // The key travels on the body of the two writes below and on nothing else:
  // never a path parameter, never a query string, so it cannot reach an access
  // log, a browser history entry or a Referer header. :vendor is anthropic or
  // openai; anything else is a 404 from the api.
  verifyProviderKey: { method: 'POST',   path: '/api/v1/providers/:vendor/verify' },
  putProviderKey:    { method: 'PUT',    path: '/api/v1/providers/:vendor/key' },
  putProviderFilter: { method: 'PUT',    path: '/api/v1/providers/:vendor/filter' },
  deleteProviderKey: { method: 'DELETE', path: '/api/v1/providers/:vendor/key' },
  // The vendor BILLING key — a separate, broader credential (it can list the
  // organisation's workspaces and keys), held by the api for its daily
  // reconciliation job and never by the gateway. Same rule: key on the body.
  verifyProviderBillingKey: { method: 'POST',   path: '/api/v1/providers/:vendor/billing-key/verify' },
  putProviderBillingKey:    { method: 'PUT',    path: '/api/v1/providers/:vendor/billing-key' },
  deleteProviderBillingKey: { method: 'DELETE', path: '/api/v1/providers/:vendor/billing-key' },

  // --- Model aliases ---
  listModelAliases: { method: 'GET',    path: '/api/v1/model-aliases' },
  putModelAlias:    { method: 'PUT',    path: '/api/v1/model-aliases/:alias' },
  testModelAlias:   { method: 'POST',   path: '/api/v1/model-aliases/:alias/test' },
  deleteModelAlias: { method: 'DELETE', path: '/api/v1/model-aliases/:alias' },

  // --- Routing policy: organisation ---
  getRoutingPolicy:    { method: 'GET', path: '/api/v1/routing-policy' },
  updateRoutingPolicy: { method: 'PUT', path: '/api/v1/routing-policy' },
  /** The platform system prompt the webchat proxy prepends to every turn. */
  getSystemPrompt:     { method: 'GET', path: '/api/v1/settings/system-prompt' },
  updateSystemPrompt:  { method: 'PUT', path: '/api/v1/settings/system-prompt' },
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

  // --- Library: skills, context and tools (every signed-in person) ---
  listLibrary:                { method: 'GET',    path: '/api/v1/library' },
  createLibraryItem:          { method: 'POST',   path: '/api/v1/library' },
  exportLibrary:              { method: 'GET',    path: '/api/v1/library/export' },
  importLibraryItem:          { method: 'POST',   path: '/api/v1/library/import' },
  getLibraryItem:             { method: 'GET',    path: '/api/v1/library/:id' },
  updateLibraryItem:          { method: 'PUT',    path: '/api/v1/library/:id' },
  deleteLibraryItem:          { method: 'DELETE', path: '/api/v1/library/:id' },
  shareLibraryItem:           { method: 'POST',   path: '/api/v1/library/:id/share' },
  exportLibraryItem:          { method: 'GET',    path: '/api/v1/library/:id/export' },
  verifyLibraryTool:          { method: 'POST',   path: '/api/v1/library/:id/tools/verify' },
  /** The bearer token travels on the body and nowhere else, like vendor keys. */
  putLibraryToolCredential:   { method: 'PUT',    path: '/api/v1/library/:id/tools/credential' },
  deleteLibraryToolCredential: { method: 'DELETE', path: '/api/v1/library/:id/tools/credential' },
  // --- Library governance (admin): org-shared items and allowed tool hosts ---
  listSharedLibrary:          { method: 'GET',    path: '/api/v1/library/shared' },
  adminUnshareLibraryItem:    { method: 'POST',   path: '/api/v1/library/shared/:id/unshare' },
  adminDeleteLibraryItem:     { method: 'DELETE', path: '/api/v1/library/shared/:id' },
  getToolHosts:               { method: 'GET',    path: '/api/v1/settings/tool-hosts' },
  updateToolHosts:            { method: 'PUT',    path: '/api/v1/settings/tool-hosts' },

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
