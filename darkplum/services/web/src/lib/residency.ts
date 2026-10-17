import type { DisplayClass, EffectivePolicy, PrincipalResidency, Residency } from '../types'

/**
 * Where requests may run — the ladder, and the one predicate that reads it.
 *
 * This is the UI's copy of a rule the gateway enforces on every request, and
 * it decides whether this product is willing to say "Runs in your
 * infrastructure" over a prompt that is on its way to a vendor's API. It lives
 * on its own, with no runtime import of any kind, for two reasons: nothing
 * else can drag a dependency into it, and scripts/check-policy.mts can load it
 * directly and assert every cell of the table rather than a re-implementation
 * of it.
 *
 * Every surface that asks "would this be refused?" — the catalogue's blocked
 * badge, an alias route's health, the picker's groups, the ladder's dead rungs
 * — asks it here, so the UI cannot disagree with itself about a rule the
 * gateway settles once.
 */

/** Rank on the ladder. Higher refuses more. Tighten-only merges compare these,
 *  never the strings. */
export const RESIDENCY_RANK: Record<Residency, number> = {
  any: 0,
  cloud_account_only: 1,
  self_hosted_only: 2,
}

/** The rank of a value that may have come off the wire from a newer server.
 *  Anything unrecognised is the strictest rung: a bundle that does not
 *  understand a rule must never render it as the permissive one. */
export function residencyRank(residency: Residency | string | null | undefined): number {
  const known = RESIDENCY_RANK[residency as Residency]
  return known === undefined ? RESIDENCY_RANK.self_hosted_only : known
}

/**
 * Which classes each rung permits. Mirrors the gateway's ResidencyPermits
 * exactly.
 *
 * 'unknown' is refused under every rung, including the loosest. A class no
 * backend has reported cannot be shown to sit inside any boundary, so `any` is
 * not a licence to guess — it permits every class we actually know about, and
 * nothing else.
 */
export function residencyPermits(
  residency: Residency | string | null | undefined,
  cls: DisplayClass,
): boolean {
  if (cls === 'unknown') return false
  switch (residencyRank(residency)) {
    case RESIDENCY_RANK.any:
      return true
    case RESIDENCY_RANK.cloud_account_only:
      return cls === 'self_hosted' || cls === 'cloud_managed'
    default:
      return cls === 'self_hosted'
  }
}

/** One rung: what it is called, and what choosing it does. */
export interface ResidencyRung {
  value: Residency
  label: string
  consequence: string
}

/** The ladder, loosest first — the order it is rendered in, because a range
 *  reads outward-in and reversing it on one screen and not the other is how
 *  two administrators end up describing one policy differently. */
export const RESIDENCY_LADDER: ResidencyRung[] = [
  {
    value: 'any',
    label: 'Any model, including outside your account',
    consequence:
      "Requests may also go to a vendor's own API — Anthropic or OpenAI — using your organisation's key. Those prompts leave your cloud account.",
  },
  {
    value: 'cloud_account_only',
    label: 'Your cloud account only',
    consequence:
      'Requests run on self-hosted models and on cloud-managed models such as AWS Bedrock. Vendor APIs are refused, even when the model is enabled.',
  },
  {
    value: 'self_hosted_only',
    label: 'Self-hosted only',
    consequence:
      'Only models running in your own cluster answer. Bedrock and vendor APIs are both refused, even when the model is enabled.',
  },
]

/** Which rung is the shipped default, named where the ladder is drawn so an
 *  administrator can tell "we chose this" from "this is what it came as". */
export const DEFAULT_RESIDENCY: Residency = 'cloud_account_only'

/** Every class the UI can name, listed here rather than imported so this module
 *  keeps its promise of no runtime dependency. */
const DISPLAY_CLASSES: DisplayClass[] = [
  'self_hosted',
  'cloud_managed',
  'external_api',
  'unknown',
]

/**
 * Choosing order for classes: what is held closest to the organisation first,
 * what leaves it last. Lower sorts earlier.
 *
 * Derived from the ladder above rather than typed out a second time, so the UI
 * has exactly one ordering of classes and it is the enforced one: a class ranks
 * by how many rungs still permit it. self_hosted survives every rung,
 * cloud_managed all but the strictest, external_api only the loosest, and
 * unknown none at all. Change the gateway's table and this follows it.
 *
 * This is what decides which model chat starts on when nobody has named a
 * default. Ordering that choice alphabetically instead is how a catalogue that
 * happens to contain "Claude …" gets a prompt out of the account before its
 * user has read the picker.
 */
export const CLASS_RANK: Record<DisplayClass, number> = Object.fromEntries(
  DISPLAY_CLASSES.map((cls) => [
    cls,
    RESIDENCY_LADDER.length - RESIDENCY_LADDER.filter((r) => residencyPermits(r.value, cls)).length,
  ]),
) as Record<DisplayClass, number>

/** Orders two classes closest-first. Equal classes are left to the caller to
 *  break — that is where the alphabetical tie-break belongs, and only there. */
export function compareClasses(a: DisplayClass, b: DisplayClass): number {
  return CLASS_RANK[a] - CLASS_RANK[b]
}

/** The two rungs a user or key may set on itself. A principal only ever
 *  narrows, so the loosest rung is not one of them. */
export const PRINCIPAL_RESIDENCIES: PrincipalResidency[] = [
  'cloud_account_only',
  'self_hosted_only',
]

/** The rung's name. Used wherever a residency is reported rather than chosen. */
export function residencyLabel(residency: Residency | string | null | undefined): string {
  const rung = RESIDENCY_LADDER.find((r) => r.value === residency)
  // Unrecognised is read as the strictest rung everywhere else, so it is named
  // as the strictest rung here too rather than shown as a raw enum value.
  return (rung ?? RESIDENCY_LADDER[RESIDENCY_LADDER.length - 1]).label
}

/** The same fact said to the person it limits, in chat. Shorter, and about
 *  where their words go rather than about what the organisation permits. */
export function residencyLabelForSelf(residency: Residency | string | null | undefined): string {
  switch (residencyRank(residency)) {
    case RESIDENCY_RANK.any:
      return 'Anywhere the organisation has enabled, including vendor APIs outside your cloud account'
    case RESIDENCY_RANK.cloud_account_only:
      return 'Inside your cloud account — nothing is sent to a model vendor'
    default:
      return 'Self-hosted only — nothing leaves your own infrastructure'
  }
}

/**
 * Whether the organisation has narrowed itself past what Darkplum ships.
 *
 * `cloud_account_only` is the shipped default and the state every install
 * starts in, so it is NOT a restriction — marking it as one would put a
 * warning on the policy card of every organisation that has never touched it,
 * and a warning everyone sees is a warning nobody reads. Only a deliberate
 * move to self-hosted only, or an allowlist, counts.
 *
 * `any` is looser than the default and so is not a restriction either; the
 * ladder says what it costs where it is chosen.
 */
export function orgPolicyRestricted(
  policy: { residency: Residency; allowed_models?: string[] | null } | null,
): boolean {
  if (policy === null) return false
  if ((policy.allowed_models ?? null) !== null) return true
  return residencyRank(policy.residency) > residencyRank(DEFAULT_RESIDENCY)
}

/**
 * Whether an effective policy takes anything away from the person it applies
 * to. When it does not, chat has nothing to explain and the "why" affordance
 * stays off the header.
 *
 * An allowlist always narrows. A residency only narrows when something it
 * refuses actually exists: `enabledClasses` is the set of classes the caller
 * can prove are enabled — the admin surfaces know the catalogue and pass it.
 *
 * Chat cannot: its model list has already been filtered, so a model the
 * residency refuses is exactly the one it cannot see. There it falls back to
 * the two things that are knowable without the catalogue — a rung stricter
 * than the shipped default, and a rung set on this person or their key rather
 * than inherited. Both are deliberate narrowings someone chose; the default,
 * inherited from the organisation, is not, and does not raise the notice.
 */
export function policyLimits(
  effective: EffectivePolicy | null,
  enabledClasses?: Iterable<DisplayClass>,
): boolean {
  if (!effective) return false
  if (effective.allowed_models !== null) return true
  const residency = effective.residency
  if (residencyRank(residency) === RESIDENCY_RANK.any) return false
  if (enabledClasses !== undefined) {
    for (const cls of enabledClasses) {
      if (!residencyPermits(residency, cls)) return true
    }
    return false
  }
  if (residencyRank(residency) > residencyRank(DEFAULT_RESIDENCY)) return true
  const source = effective.sources?.residency
  return source === 'user' || source === 'api_key'
}
