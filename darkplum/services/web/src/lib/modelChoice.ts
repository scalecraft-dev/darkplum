/**
 * Which model chat starts on, and what the empty state is willing to claim
 * about it.
 *
 * Both answers come from one fact: where a prompt on this entry actually goes.
 * Nothing here picks by name. A catalogue holding a self-hosted model and a
 * vendor one sorts "Claude …" first alphabetically, and a picker that opens on
 * whatever is first in the list will send someone's first message out of the
 * account before they have looked at the header. The class decides; the name
 * only breaks ties inside a class.
 *
 * The imports carry their `.ts` extension so scripts/check-policy.mts can load
 * this module directly under `node --experimental-strip-types` and assert the
 * real function rather than a copy of it — the same reason residency.ts keeps
 * itself free of runtime dependencies.
 */

import type { AliasKind, BackendClass, DisplayClass } from '../types'
import { CLASS_RANK, compareClasses } from './residency.ts'
import { displayClassOf, vendorLabel } from './modelRouting.ts'

/** A picker entry whose target is chosen per message. `ModelAlias` spells this
 *  field `kind` and has isRoutedAlias for it; a catalogue entry spells it
 *  `alias_kind`, which is what the picker and chat both read. */
function isRouted(m: { alias_kind?: AliasKind | null }): boolean {
  return m.alias_kind === 'routed'
}

/** The parts of a picker entry this module reads. Narrower than `Model` so a
 *  caller — and the check script — can pass the fields that matter. */
export interface ChoosableModel {
  id: string
  /** On an external model this is the vendor, and it is the only place the
   *  vendor's name can be read from. */
  owned_by?: string
  backend_class?: BackendClass | string | null
  alias_kind?: AliasKind | null
  /** Chat-facing route summaries: the wire field is `backend_class`. */
  routes?: { backend_class?: BackendClass | string | null }[] | null
}

/**
 * Every place a prompt on this entry could end up.
 *
 * One class for a concrete model. For a routed alias it is every class its
 * routes declare, because which route answers is decided per message — the
 * entry's own class only describes the default route's target. A route that
 * names no class counts as 'unknown', never as the default route's class. Only
 * when the API sent no routes at all is the entry's own class all there is.
 */
function destinationClasses(m: ChoosableModel): DisplayClass[] {
  if (isRouted(m)) {
    const routes = m.routes ?? []
    if (routes.length > 0) return routes.map((r) => displayClassOf({ backend_class: r.backend_class }))
  }
  return [displayClassOf(m)]
}

/**
 * The least contained place this entry can send a prompt.
 *
 * Worst case, never likeliest: an alias whose default route is self-hosted but
 * whose second route reaches Anthropic is ranked as, and spoken about as, the
 * one that reaches Anthropic.
 */
export function promptDestinationClass(m: ChoosableModel): DisplayClass {
  return destinationClasses(m).reduce((worst, cls) =>
    CLASS_RANK[cls] > CLASS_RANK[worst] ? cls : worst,
  )
}

/**
 * The entry chat opens on.
 *
 * An administrator's default — whatever the API resolved walking key → user →
 * organisation — wins whenever it is still on offer, including when it is a
 * vendor model: someone chose that deliberately and this is not the place to
 * second-guess it. A conversation's own model is handled by its caller and
 * outranks this entirely; this only answers the first visit.
 *
 * With no default, the closest-held class wins and names break ties only inside
 * a class. Returns '' for an empty list, which reads as "nothing selected"
 * everywhere the picker is drawn.
 */
export function pickDefaultModel(
  models: readonly ChoosableModel[],
  defaultModel?: string | null,
): string {
  if (models.length === 0) return ''
  if (defaultModel && models.some((m) => m.id === defaultModel)) return defaultModel
  let best = models[0]
  let bestClass = promptDestinationClass(best)
  for (const m of models.slice(1)) {
    const cls = promptDestinationClass(m)
    const byClass = compareClasses(cls, bestClass)
    if (byClass < 0 || (byClass === 0 && m.id.localeCompare(best.id) < 0)) {
      best = m
      bestClass = cls
    }
  }
  return best.id
}

/** What the empty state says when it cannot place the selected entry: true of
 *  every install, and claims no boundary the prompt may not stay inside. */
const NEUTRAL_TAGLINE = 'Private AI for your organisation.'

/**
 * The one line under the wordmark on an empty thread.
 *
 * It sits directly above a picker that may be saying "Sent to Anthropic —
 * leaves your cloud account", so it follows the selected entry rather than
 * asserting the self-hosted case over all of them. A routed alias that could
 * land in more than one place, an unreported class, and nothing selected all
 * get the neutral line: the hero of an empty page is the last place to guess.
 */
export function chatTagline(m: ChoosableModel | null | undefined): string {
  if (!m) return NEUTRAL_TAGLINE
  const classes = destinationClasses(m)
  if (classes.some((c) => c !== classes[0])) return NEUTRAL_TAGLINE
  switch (promptDestinationClass(m)) {
    case 'self_hosted':
      return 'Private AI, running in your infrastructure.'
    case 'cloud_managed':
      return 'Private AI, running in your cloud account.'
    case 'external_api': {
      // Routes carry a class, not a provider, so a routed alias has no vendor
      // to name and says the part it can stand behind instead.
      const vendor = isRouted(m) ? null : m.owned_by
      return vendor
        ? `This model is called at ${vendorLabel(vendor)}. Your message leaves your cloud account.`
        : 'Your message leaves your cloud account.'
    }
    default:
      return NEUTRAL_TAGLINE
  }
}
