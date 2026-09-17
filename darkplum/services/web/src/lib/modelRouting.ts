import type { BackendClass } from '../types'

/** Alias format enforced by the API and the database check constraint. */
export const ALIAS_PATTERN = '^[a-z0-9][a-z0-9._-]{0,62}$'
const ALIAS_RE = new RegExp(ALIAS_PATTERN)

export const ALIAS_HINT =
  'Lowercase letters, numbers, dots, dashes and underscores. Starts with a letter or number, up to 63 characters.'

/**
 * Returns a message explaining why the alias is unusable, or null when valid.
 * The messages name the specific problem rather than repeating the pattern.
 */
export function validateAlias(alias: string): string | null {
  if (!alias) return 'Enter a name for the alias.'
  if (alias.length > 63) return 'Use 63 characters or fewer.'
  if (/[A-Z]/.test(alias)) return 'Use lowercase letters only.'
  if (!/^[a-z0-9]/.test(alias)) return 'Start with a letter or a number.'
  if (!ALIAS_RE.test(alias)) return 'Use only letters, numbers, dots, dashes and underscores.'
  return null
}

/** Older APIs omit backend_class; everything we ship in-cluster is self-hosted. */
export function backendClassOf(m: { backend_class?: BackendClass | null }): BackendClass {
  return m.backend_class === 'cloud_managed' ? 'cloud_managed' : 'self_hosted'
}

/** Older APIs omit `available`; only an explicit false means unreachable. */
export function isReachable(m: { available?: boolean | null }): boolean {
  return m.available !== false
}

/** Same rule for the target of an alias. */
export function isTargetReachable(a: { target_available?: boolean | null }): boolean {
  return a.target_available !== false
}

/** One sentence, used wherever an unreachable model needs explaining. */
export const UNREACHABLE_HINT =
  'Not reachable right now — it stays configured and returns when its backend is back.'

/** Admin vocabulary — matches the wording on the routing policy control. */
export function backendClassLabel(c: BackendClass): string {
  return c === 'cloud_managed' ? 'Cloud-managed' : 'Self-hosted'
}

/** End-user vocabulary — answers "where does my conversation go?". */
export function backendClassChatLabel(c: BackendClass): string {
  return c === 'cloud_managed' ? 'Runs in your cloud provider' : 'Runs in your infrastructure'
}
