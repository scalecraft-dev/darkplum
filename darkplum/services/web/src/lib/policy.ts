import type {
  AdminModel,
  EffectivePolicy,
  ModelAlias,
  PolicySource,
  PrincipalPolicy,
  Residency,
} from '../types'
import { formatModelName } from './formatModelName'
import { aliasTargets } from './modelRouting'

/**
 * The words used wherever per-principal policy appears — the drawer, the
 * organisation card, the access overview and the chat explanation. Said the
 * same way every time, because four different phrasings of one rule reads as
 * four different rules.
 */

/** The rule, in one line. Shown wherever a policy is being written. */
export const POLICY_RULE =
  'A key can never reach more than its owner. A user never more than the organisation.'

/**
 * When a change starts being enforced. The gateway holds a snapshot of the
 * policy and rebuilds it periodically, so a tightening is not instantaneous and
 * saying otherwise would be a lie an administrator could catch.
 */
export const POLICY_TIMING =
  'Takes effect within a few seconds, once the gateway refreshes its policy snapshot. Revoking a key is immediate.'

export const INHERIT_LABEL = 'Inherit'

export function residencyLabel(residency: Residency): string {
  return residency === 'self_hosted_only' ? 'Self-hosted only' : 'Any model'
}

/** Where a field's value came from, named as a level rather than a column. */
export function sourceLabel(source: PolicySource): string {
  switch (source) {
    case 'org':
      return 'From the organisation'
    case 'user':
      return "From this user's policy"
    case 'api_key':
      return "From this key's policy"
    default:
      return 'Not set anywhere'
  }
}

/** The short form, for a chip beside the value it explains. */
export function sourceChip(source: PolicySource): string {
  switch (source) {
    case 'org':
      return 'Organisation'
    case 'user':
      return 'User'
    case 'api_key':
      return 'API key'
    default:
      return 'Unset'
  }
}

/** A model id in the words a person picked it by. Falls back to the id, which
 *  is also what a policy stores, so nothing is ever unidentifiable. */
export function modelLabel(id: string, models: AdminModel[]): string {
  const match = models.find((m) => m.model_id === id || m.id === id)
  if (!match) return id
  return match.display_name ?? formatModelName(match.model_id)
}

/** An empty policy row: every field inherits. */
export const INHERIT_ALL: PrincipalPolicy = {
  residency: null,
  allowed_models: null,
  default_model: null,
}

export function isInheritAll(policy: PrincipalPolicy): boolean {
  return (
    policy.residency === null &&
    policy.allowed_models === null &&
    policy.default_model === null
  )
}

/** Two policy rows are the same when all three fields agree, order included —
 *  the allowlist is sorted before it is sent, so order is meaningful here. */
export function samePolicy(a: PrincipalPolicy, b: PrincipalPolicy): boolean {
  if (a.residency !== b.residency) return false
  if (a.default_model !== b.default_model) return false
  if (a.allowed_models === null || b.allowed_models === null) {
    return a.allowed_models === b.allowed_models
  }
  if (a.allowed_models.length !== b.allowed_models.length) return false
  return a.allowed_models.every((id, i) => id === b.allowed_models![i])
}

/** One readable line for a stored row in a list — what the principal changed,
 *  not what it ends up with. The effective set is shown beside it. */
export function storedSummary(policy: PrincipalPolicy, models: AdminModel[]): string {
  const parts: string[] = []
  if (policy.residency === 'self_hosted_only') parts.push('Self-hosted only')
  if (policy.allowed_models !== null) {
    const n = policy.allowed_models.length
    parts.push(`${n} ${n === 1 ? 'model' : 'models'} allowed`)
  }
  if (policy.default_model) parts.push(`Default ${modelLabel(policy.default_model, models)}`)
  return parts.length > 0 ? parts.join(' · ') : 'Inherits everything'
}

/**
 * Whether an effective policy narrows anything at all. When it does not, chat
 * has nothing to explain and the "why" affordance stays off the header.
 */
export function policyLimits(effective: EffectivePolicy | null): boolean {
  if (!effective) return false
  return effective.allowed_models !== null || effective.residency === 'self_hosted_only'
}

/**
 * The concrete model ids a routed or static alias can reach. Allowlists take
 * model ids, never aliases, so the helper that offers "add every target of this
 * alias" expands at the moment it is clicked — a later edit to the alias does
 * not quietly widen a policy that was written against it.
 */
export function aliasTargetIds(alias: ModelAlias): string[] {
  return [...new Set(aliasTargets(alias))].sort()
}
