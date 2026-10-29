/**
 * Request parameters the gateway did not pass on as sent (GitHub #3), and the
 * organisation-wide switch that decides whether it drops them or refuses.
 *
 * The usage log's `dropped_params` holds the entries of the
 * X-Darkplum-Dropped-Params response header, verbatim:
 *
 *   seed                                  dropped (not sent)
 *   stream_options.include_obfuscation    dropped (nested key)
 *   max_tokens=raised                     adjusted, not dropped
 *   unknown_keys=more_7                   7 more unknown keys past the cap of 20
 *
 * Names only, never values. An unknown key's NAME is chosen by the caller, so
 * it is rendered as text and capped in the badge, never trusted for layout.
 * Pure functions, pinned by scripts/check-policy.mts.
 */

export type ParamsMode = 'lenient' | 'strict'

/** A value this bundle does not recognise reads as lenient: that is what the
 *  gateway does with a NULL column, and the card must show what is enforced. */
export function paramsModeOf(v: string | null | undefined): ParamsMode {
  return v === 'strict' ? 'strict' : 'lenient'
}

/** The two options, worded as what happens to a request. */
export const PARAMS_MODE_OPTIONS: ReadonlyArray<{
  value: ParamsMode
  label: string
  consequence: string
}> = [
  {
    value: 'lenient',
    label: "Drop and report parameters a model can't use",
    consequence:
      'The request is answered without them, and each one is named in the response header and on the Activity log. Dropping seed or a penalty changes the output.',
  },
  {
    value: 'strict',
    label: 'Refuse requests that include them',
    consequence:
      'Every key and user in the organisation gets a 400 naming the parameter instead. Clients that send SDK extras will start failing.',
  },
]

/** Said once under the two options: what neither option changes, and where it
 *  applies. */
export const PARAMS_MODE_NOTE =
  'Some parameters are always refused whichever is chosen, because dropping them would return a wrong answer (JSON mode, n above 1, a forced tool call). Takes effect on gateways running this release.'

/** The confirmation for turning strict on. Turning it off is not confirmed:
 *  nothing that is answered today starts failing. `alsoLine` is the sentence
 *  added to a residency or allowlist confirmation saved in the same go. */
export const PARAMS_MODE_STRICT_CONFIRM = {
  title: "Refuse requests with parameters a model can't use?",
  description: 'This applies to every API key and every user in the organisation at once.',
  body: [
    "A request that sends a parameter its model can't use (seed or a penalty to an Anthropic model, a key the gateway does not know, an SDK extra) gets a 400 naming that parameter instead of an answer.",
    'Requests that are answered today can start failing. The Activity log marks every request that had something dropped, so check it for who sends them first.',
  ],
  action: 'Refuse them',
  alsoLine:
    "This also turns on strict mode: requests that send a parameter their model can't use will be refused with a 400, for every key and user.",
} as const

/** The short chip in the card header for the stored mode. */
export function paramsModeChip(mode: ParamsMode): string {
  return mode === 'strict' ? 'Parameters: refused' : 'Parameters: dropped and reported'
}

// ---------------------------------------------------------------------------
// The usage log's dropped_params
// ---------------------------------------------------------------------------

/** The badge never shows more than this many names; the title has them all. */
export const DROPPED_BADGE_NAMES = 2
/** An unknown key's name is caller-chosen and can be any length. */
export const DROPPED_NAME_MAX = 24

const MORE_UNKNOWN = /^unknown_keys=more_(\d+)$/

/** The adjustments the gateway records as name=action. Anything else with an
 *  "=" in it is read as a bare dropped name: the gateway already rewrites "="
 *  in a caller's key name to "_" (sanitizeParamName), and this keeps a name
 *  that somehow got through from being re-worded as something the gateway
 *  did. */
export const ADJUSTMENT_ACTIONS: ReadonlySet<string> = new Set(['raised'])

interface Parsed {
  /** Entries that were not sent: bare names. */
  dropped: string[]
  /** name=action entries, as "raised max_tokens". */
  adjusted: string[]
  /** Unknown keys past the gateway's cap of 20, counted but not named. */
  moreUnknown: number
}

function parse(entries: readonly string[] | null | undefined): Parsed {
  const out: Parsed = { dropped: [], adjusted: [], moreUnknown: 0 }
  for (const raw of entries ?? []) {
    const entry = raw.trim()
    if (!entry) continue
    const more = MORE_UNKNOWN.exec(entry)
    if (more) {
      out.moreUnknown += Number(more[1])
      continue
    }
    const eq = entry.indexOf('=')
    const action = eq > 0 ? entry.slice(eq + 1) : ''
    if (ADJUSTMENT_ACTIONS.has(action)) {
      out.adjusted.push(`${action} ${entry.slice(0, eq)}`)
    } else {
      out.dropped.push(entry)
    }
  }
  return out
}

function cap(name: string): string {
  return name.length > DROPPED_NAME_MAX ? `${name.slice(0, DROPPED_NAME_MAX - 1)}…` : name
}

/**
 * "Dropped · seed, logprobs +2", or null when nothing was dropped or adjusted.
 * Drops are named before adjustments; a row with only adjustments reads
 * "Adjusted · raised max_tokens", because nothing was dropped from it.
 */
export function droppedParamsBadge(entries: readonly string[] | null | undefined): string | null {
  const p = parse(entries)
  const named = [...p.dropped, ...p.adjusted]
  const total = named.length + p.moreUnknown
  if (total === 0) return null
  const verb = p.dropped.length === 0 && p.moreUnknown === 0 ? 'Adjusted' : 'Dropped'
  const shown = named.slice(0, DROPPED_BADGE_NAMES).map(cap)
  const rest = total - shown.length
  if (shown.length === 0) return `${verb} · ${rest} unknown ${rest === 1 ? 'key' : 'keys'}`
  return `${verb} · ${shown.join(', ')}${rest > 0 ? ` +${rest}` : ''}`
}

/** The badge's title: every entry, in a sentence. Null when there is none. */
export function droppedParamsTitle(entries: readonly string[] | null | undefined): string | null {
  const p = parse(entries)
  const parts: string[] = []
  if (p.dropped.length > 0 || p.moreUnknown > 0) {
    const names = [...p.dropped]
    if (p.moreUnknown > 0) {
      names.push(`${p.moreUnknown} more unknown ${p.moreUnknown === 1 ? 'key' : 'keys'}`)
    }
    parts.push(`Not sent to the model: ${names.join(', ')}.`)
  }
  if (p.adjusted.length > 0) {
    parts.push(`Changed by the gateway: ${p.adjusted.join(', ')}.`)
  }
  if (parts.length === 0) return null
  parts.push(
    p.dropped.length > 0 || p.moreUnknown > 0
      ? 'The request was answered without them, so the output may differ from what the caller asked for.'
      : 'The request was answered with that change.',
  )
  return parts.join(' ')
}
