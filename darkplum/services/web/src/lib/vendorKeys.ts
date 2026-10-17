import type { ProviderState, ProviderStatus, VendorName } from '../types'

/**
 * Installing a vendor key from the UI: the decisions that have to be the same
 * every time, kept out of the components that show them.
 *
 * Everything here is pure and asserted by scripts/check-policy.mts. Three of
 * these functions decide something a component cannot check for itself:
 *
 *   - which patterns are written into a vendor's model filter, which decides
 *     which models the organisation is billed for;
 *   - what the key entry line says about a key nobody can see;
 *   - whether a vendor is still installing or has stopped trying.
 *
 * None of them ever receives the key except keyFingerprint/vendorKeyProblem,
 * which take it as an argument, return a description of it, and keep nothing.
 */

// ---------------------------------------------------------------------------
// Glob matching — a port of Go's path.Match
//
// The gateway filters a vendor's discovered ids with path.Match
// (services/gateway/internal/routing/filter.go). This UI decides whether a
// pattern it is about to write admits exactly the models an administrator
// ticked, so it has to answer that question the way the gateway will, not
// approximately. A prefix comparison would be right for `claude-sonnet*` and
// wrong for any id containing a separator, and the failure mode is a ticked
// model quietly missing from the catalogue.
//
// Ported from the Go standard library's path.Match, chunk for chunk.
// ---------------------------------------------------------------------------

/** Thrown by neither function here — a malformed pattern is reported as "does
 *  not match", the same answer the caller wants, and separately refused by
 *  isWritablePattern below. Kept as a distinct symbol for the port's shape. */
const BAD_PATTERN = Symbol('bad pattern')

type ChunkResult = { rest: string; matched: boolean } | typeof BAD_PATTERN

/** Splits the pattern at the first '*', returning whether it began with one. */
function scanChunk(pattern: string): { star: boolean; chunk: string; rest: string } {
  let star = false
  while (pattern.startsWith('*')) {
    pattern = pattern.slice(1)
    star = true
  }
  let inRange = false
  let i = 0
  for (; i < pattern.length; i += 1) {
    const c = pattern[i]
    if (c === '\\') {
      if (i + 1 < pattern.length) i += 1
    } else if (c === '[') {
      inRange = true
    } else if (c === ']') {
      inRange = false
    } else if (c === '*' && !inRange) {
      break
    }
  }
  return { star, chunk: pattern.slice(0, i), rest: pattern.slice(i) }
}

/** Matches the leading literal/class run of a chunk against the name. */
function matchChunk(chunk: string, s: string): ChunkResult {
  // A failed match still has to walk the rest of the chunk to decide whether
  // the pattern is syntactically valid, exactly as the Go version does.
  let failed = false
  while (chunk.length > 0) {
    if (!failed && s.length === 0) failed = true
    if (chunk[0] === '[') {
      let r = ''
      if (!failed) {
        r = s[0]
        s = s.slice(1)
      }
      let rest = chunk.slice(1)
      let negated = false
      if (rest.startsWith('^')) {
        negated = true
        rest = rest.slice(1)
      }
      let match = false
      let nrange = 0
      for (;;) {
        if (rest.startsWith(']') && nrange > 0) {
          rest = rest.slice(1)
          break
        }
        if (rest.length === 0) return BAD_PATTERN
        const lo = getEsc(rest)
        if (lo === BAD_PATTERN) return BAD_PATTERN
        let hi = lo.char
        rest = lo.rest
        if (rest.startsWith('-')) {
          const next = getEsc(rest.slice(1))
          if (next === BAD_PATTERN) return BAD_PATTERN
          hi = next.char
          rest = next.rest
        }
        if (lo.char <= r && r <= hi) match = true
        nrange += 1
      }
      if (match === negated) failed = true
      chunk = rest
      continue
    }
    if (chunk[0] === '?') {
      if (!failed) {
        if (s[0] === '/') failed = true
        else s = s.slice(1)
      }
      chunk = chunk.slice(1)
      continue
    }
    if (chunk[0] === '\\') {
      chunk = chunk.slice(1)
      if (chunk.length === 0) return BAD_PATTERN
    }
    if (!failed) {
      if (chunk[0] !== s[0]) failed = true
      else s = s.slice(1)
    }
    chunk = chunk.slice(1)
  }
  return failed ? { rest: '', matched: false } : { rest: s, matched: true }
}

function getEsc(chunk: string): { char: string; rest: string } | typeof BAD_PATTERN {
  if (chunk.length === 0 || chunk[0] === '-' || chunk[0] === ']') return BAD_PATTERN
  let rest = chunk
  if (rest[0] === '\\') {
    rest = rest.slice(1)
    if (rest.length === 0) return BAD_PATTERN
  }
  const char = rest[0]
  rest = rest.slice(1)
  if (rest.length === 0) return BAD_PATTERN
  return { char, rest }
}

/** True when `name` is admitted by `pattern`, as Go's path.Match decides it.
 *  A malformed pattern never matches. */
export function globMatches(pattern: string, name: string): boolean {
  let pat = pattern
  let s = name
  while (pat.length > 0) {
    const { star, chunk, rest } = scanChunk(pat)
    pat = rest
    if (star && chunk === '') {
      // A trailing star takes the rest of the name, but never past a separator.
      return !s.includes('/')
    }
    const res = matchChunk(chunk, s)
    if (res === BAD_PATTERN) return false
    if (res.matched && (res.rest.length === 0 || pat.length > 0)) {
      s = res.rest
      continue
    }
    if (star) {
      let advanced = false
      for (let i = 0; i < s.length && s[i] !== '/'; i += 1) {
        const skipped = matchChunk(chunk, s.slice(i + 1))
        if (skipped === BAD_PATTERN) return false
        if (skipped.matched) {
          if (pat.length === 0 && skipped.rest.length > 0) continue
          s = skipped.rest
          advanced = true
          break
        }
      }
      if (advanced) continue
    }
    return false
  }
  return s.length === 0
}

/** Everything in `ids` that any of `patterns` admits, in the order of `ids`.
 *  An empty pattern list admits nothing here — unlike the gateway, where an
 *  empty vendor filter means "offer nothing" rather than "offer everything". */
export function globAdmits(patterns: readonly string[], ids: readonly string[]): string[] {
  return ids.filter((id) => patterns.some((p) => globMatches(p, id)))
}

/** A pattern made only of wildcards admits the vendor's entire catalogue,
 *  now and for every model they publish later. The api refuses one without
 *  allow_all, and this UI has no way to ask for allow_all, so nothing here may
 *  produce one. */
export function isBareWildcard(pattern: string): boolean {
  return pattern.length > 0 && /^[*?]+$/.test(pattern)
}

/** Escapes a model id so it is matched literally: a metacharacter in an id
 *  would otherwise turn an exact selection into a pattern, or make the whole
 *  filter unparseable at the gateway. */
export function escapeGlobLiteral(id: string): string {
  return id.replace(/([*?[\\\]])/g, '\\$1')
}

// ---------------------------------------------------------------------------
// The model filter
// ---------------------------------------------------------------------------

/**
 * The family a model id belongs to: everything before its first version-ish
 * segment. claude-sonnet-4-5-20250929 -> claude-sonnet; gpt-4o-mini -> gpt;
 * o4-mini -> o4-mini (nothing to strip).
 *
 * A heuristic, and deliberately allowed to be a coarse one: grouping only
 * proposes a glob. Whether a glob is written is decided below by checking what
 * it admits against the reachable list, so a family that is too wide simply
 * falls back to exact ids instead of admitting a model nobody ticked.
 */
export function familyOf(id: string): string {
  const parts = id.split('-')
  const cut = parts.findIndex((p, i) => i > 0 && /^\d/.test(p))
  return cut === -1 ? id : parts.slice(0, cut).join('-')
}

/**
 * The checklist's headings.
 *
 * familyOf is a heuristic over ids, and on some catalogues it produces a
 * heading per model: `claude-429`, `claude-notemp`, `claude-slow` are three
 * families of one, and three headings over one row each is noise that hides
 * the two real families underneath it.
 *
 * So: a family of one keeps its heading while most families are real, and is
 * swept into one neutral group per vendor when most of them are not. "Most" is
 * counted, not guessed — more families of one than families of several, and at
 * least two of them, because a single lone model is better described by its own
 * name than by "Other".
 *
 * `family: null` is the swept group; the caller names it after the vendor.
 * Grouping is presentation only — serialiseModelFilter decides what is written
 * from the ticks, not from these headings.
 */
export interface PickerGroup {
  /** The family these ids share, or null for the vendor's leftovers. */
  family: string | null
  ids: string[]
}

export function groupAdmittedModels(ids: readonly string[]): PickerGroup[] {
  const order: string[] = []
  const members = new Map<string, string[]>()
  for (const id of ids) {
    const family = familyOf(id)
    if (!members.has(family)) {
      members.set(family, [])
      order.push(family)
    }
    members.get(family)!.push(id)
  }

  const singles = order.filter((f) => members.get(f)!.length === 1)
  const multi = order.filter((f) => members.get(f)!.length > 1)
  if (singles.length < 2 || singles.length <= multi.length) {
    return order.map((family) => ({ family, ids: members.get(family)! }))
  }

  const groups: PickerGroup[] = multi.map((family) => ({ family, ids: members.get(family)! }))
  // The leftovers stay in the vendor's own order, after the named families.
  groups.push({ family: null, ids: singles.map((f) => members.get(f)![0]) })
  return groups
}

/**
 * Which models a checklist opens with.
 *
 * Install has nothing to start from and opens with everything ticked: someone
 * who opened "Install" wants this vendor, and the list is there to be trimmed.
 *
 * Every other case starts from what this vendor offers today, and that is the
 * part worth stating plainly, because getting it wrong widens a filter nobody
 * touched: a Replace that opens with everything ticked turns a five-model
 * selection into whatever the new key can reach, and the only screen that says
 * so is the one being clicked past.
 *
 * "What it offers today" is resolved against the reachable list the way the
 * gateway resolves it — `currentFilter` may hold globs rather than ids, so
 * `claude-sonnet*` has to be matched, not compared. The catalogued ids are
 * unioned in: a filter written from the command line and since changed still
 * leaves rows on offer, and those are on offer whatever the patterns say.
 *
 * A second check within one modal session keeps the ticks from the first
 * (`previous`), so editing the key does not undo a selection. And if none of
 * the baseline survives — a new key that cannot reach any of it — everything
 * admitted is ticked, because an empty checklist has no Install button.
 */
export function seedTickedModels(params: {
  /** What the key just checked can offer. */
  admitted: readonly string[]
  /** Everything that key answers with, before the filter narrows it. */
  reachable: readonly string[]
  /** The patterns in force for this vendor now. Globs, ids, or empty. */
  currentFilter: readonly string[]
  /** This vendor's catalogued ids now. Empty on a first install. */
  currentModelIds: readonly string[]
  /** Ticks from an earlier check in the same modal session. */
  previous: ReadonlySet<string>
}): Set<string> {
  const { admitted, reachable, currentFilter, currentModelIds, previous } = params

  let baseline: ReadonlySet<string>
  if (previous.size > 0) {
    baseline = previous
  } else {
    // The filter in force IS the current selection. The catalogue is only a
    // fallback for when no filter is known: its rows outlive the filter that
    // admitted them (a model that was unticked stays listed, unavailable, until
    // it ages out), so unioning it in would quietly re-tick what an
    // administrator just removed.
    baseline =
      currentFilter.length > 0
        ? new Set(globAdmits(currentFilter, reachable))
        : new Set(currentModelIds)
  }
  if (baseline.size === 0) return new Set(admitted)

  const kept = admitted.filter((id) => baseline.has(id))
  return new Set(kept.length > 0 ? kept : admitted)
}

/**
 * The `model_filter` to send for a set of ticked models.
 *
 * A family glob is written only when, against the reachable list, it admits
 * exactly that family's ticked ids and nothing else. Otherwise the family's
 * ticked ids are written out one by one. A family of one never becomes a glob:
 * there is no family to describe, and the glob would only widen what a future
 * model release is admitted into.
 *
 * The result is checked against the reachable list before it is returned. If
 * the patterns would admit anything other than exactly the ticked set, every
 * id is written exactly instead — a filter that is longer than it needed to be
 * is a cosmetic problem; one that admits a model nobody chose is a bill.
 */
export function serialiseModelFilter(
  reachable: readonly string[],
  ticked: ReadonlySet<string>,
): string[] {
  const chosen = reachable.filter((id) => ticked.has(id))
  if (chosen.length === 0) return []

  // Families in the order their first member appears, so the filter reads in
  // the same order as the checklist that produced it.
  const families: string[] = []
  const members = new Map<string, string[]>()
  for (const id of reachable) {
    const family = familyOf(id)
    if (!members.has(family)) {
      members.set(family, [])
      families.push(family)
    }
    members.get(family)!.push(id)
  }

  const patterns: string[] = []
  for (const family of families) {
    const all = members.get(family)!
    const wanted = all.filter((id) => ticked.has(id))
    if (wanted.length === 0) continue

    const glob = `${family}*`
    const usable =
      all.length > 1 &&
      !isBareWildcard(glob) &&
      sameSet(globAdmits([glob], reachable), wanted)
    if (usable) patterns.push(glob)
    else for (const id of wanted) patterns.push(escapeGlobLiteral(id))
  }

  // Last check, against the same list the gateway will filter.
  if (!sameSet(globAdmits(patterns, reachable), chosen)) {
    return chosen.map(escapeGlobLiteral)
  }
  return patterns
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false
  const set = new Set(a)
  return b.every((x) => set.has(x))
}

/** What changed between the models on offer before a filter change and after
 *  it. Both lists come from the api — `admitted` on the response — so this
 *  describes a decision the server already made, never a predicted one. */
export interface AdmittedDiff {
  added: string[]
  removed: string[]
  /** How many were on offer before and still are. */
  kept: number
  /** Nothing moved. The modal says so rather than reporting an empty change. */
  unchanged: boolean
}

export function admittedDiff(
  before: readonly string[],
  after: readonly string[],
): AdmittedDiff {
  const had = new Set(before)
  const has = new Set(after)
  const added = after.filter((id) => !had.has(id))
  const removed = before.filter((id) => !has.has(id))
  return {
    added,
    removed,
    kept: after.length - added.length,
    unchanged: added.length === 0 && removed.length === 0,
  }
}

// ---------------------------------------------------------------------------
// The key itself
// ---------------------------------------------------------------------------

/** Format the api enforces before it spends a request on verifying the key. */
const KEY_RE = /^[A-Za-z0-9_-]{20,512}$/

/**
 * What an admin reads when the vendor ANSWERED the key check with something
 * other than 401/403 (code `vendor_error`).
 *
 * The api's own closed sentence is used verbatim when present; the pointer
 * after it is ours, and exists because this is the one failure that is not a
 * network problem and must not send anyone to the network runbook. Status and
 * type arrive from the api already validated, and are re-checked here so a
 * malformed body falls back to a sentence that claims nothing it cannot back.
 */
export function vendorErrorCopy(
  vendorName: string,
  serverMessage: string,
  status: number | null,
  errorType: string | null,
): string {
  const type = errorType && /^[a-z_]{1,40}$/.test(errorType) ? errorType : 'unknown'
  const statusText = status !== null && Number.isInteger(status) && status >= 100 && status <= 599 ? `HTTP ${status}` : 'an HTTP error'
  const lead = serverMessage.trim()
    ? serverMessage.trim()
    : `${vendorName} answered ${statusText} (${type}) to the key check. Nothing was stored.`
  return `${lead} This is ${vendorName}'s answer to this key, not a network problem \u2014 check the key's organisation and permissions in ${vendorName}'s console.`
}

/**
 * Why this key cannot be sent, or null when it can.
 *
 * The messages describe the key without ever quoting it: what is wrong with a
 * pasted key is nearly always its shape — a stray quotation mark, a trailing
 * newline out of a terminal — and naming that is the whole of the help needed.
 */
export function vendorKeyProblem(key: string): string | null {
  const k = key.trim()
  if (k === '') return 'Paste the key first.'
  if (/\s/.test(k)) {
    return 'There is a space or a line break inside this key. Copy it again without the surrounding text.'
  }
  if (k.length < 20) return 'That is shorter than any vendor key — they are at least 20 characters.'
  if (k.length > 512) return 'That is longer than 512 characters, which is longer than any vendor key.'
  if (!KEY_RE.test(k)) {
    return 'A vendor key is letters, numbers, dashes and underscores only. Quotation marks picked up with the paste are the usual cause.'
  }
  return null
}

// ---------------------------------------------------------------------------
// The Anthropic workspace id
// ---------------------------------------------------------------------------

/**
 * Whether a vendor's key form asks for a workspace id.
 *
 * Anthropic refuses an organisation-level key unless every request names the
 * workspace it is to be billed and scoped to (`anthropic-workspace-id`). A key
 * created inside a workspace needs nothing. OpenAI's organisation and project
 * headers are a different mechanism and are not asked for here.
 */
export function vendorTakesWorkspaceId(vendor: VendorName): boolean {
  return vendor === 'anthropic'
}

/** The api's own rule for the id, mirrored so a typo is caught before a
 *  request is spent on it. Not a secret: it is a name, shown on the card. */
export const WORKSPACE_ID_RE = /^[A-Za-z0-9_-]{1,64}$/

/** Why this workspace id cannot be sent, or null when it can. Empty is valid:
 *  the field is optional, and an empty value clears a stored one. */
export function workspaceIdProblem(raw: string): string | null {
  const w = raw.trim()
  if (w === '') return null
  if (/\s/.test(w)) return 'There is a space inside this workspace ID. Copy it again from the console.'
  if (w.length > 64) return 'A workspace ID is at most 64 characters.'
  if (!WORKSPACE_ID_RE.test(w)) {
    return 'A workspace ID is letters, numbers, dashes and underscores only, like wrkspc_01AbC.'
  }
  return null
}

/**
 * The `workspace_id` to put on a request, or undefined to leave it off.
 *
 * The api reads absent as "keep what is stored" and "" as "clear it". Left off
 * only when there is nothing to say either way — a vendor that takes no
 * workspace, or an empty field over nothing stored. Otherwise the field's value
 * is sent as it stands, so a check is made against exactly what is on screen.
 */
export function workspaceIdToSend(
  vendor: VendorName,
  raw: string,
  current: string | null | undefined,
): string | undefined {
  if (!vendorTakesWorkspaceId(vendor)) return undefined
  const w = raw.trim()
  if (w === '' && !current) return undefined
  return w
}

/** The fallback for a `workspace_required` body with no message — the api's
 *  own sentence, so the screen reads the same either way. */
export const WORKSPACE_REQUIRED_MESSAGE =
  'This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace.'

/** What is shown under the workspace field on a `workspace_required` 422: the
 *  server's sentence verbatim, then the way out that needs no workspace id. */
export function workspaceRequiredCopy(serverMessage: string): string {
  const lead = serverMessage.trim() || WORKSPACE_REQUIRED_MESSAGE
  return `${lead} Keys created inside a workspace don't need this.`
}

/** How many characters of a key are shown at the end of the fingerprint line.
 *  Four is what a vendor's own console shows beside a key, so it is the number
 *  an administrator can actually check against something. */
const KEY_TAIL = 4

/**
 * What the entry field says about a key that is never displayed.
 *
 * There is no show/hide toggle: revealing the field puts the key on a screen
 * that may be shared, and the only question someone actually has — "did the
 * whole thing paste?" — is answered better by a length and a tail than by the
 * key itself.
 *
 * `spoken` is the same fact without the leader dots, which a screen reader
 * would otherwise announce one at a time.
 */
export function keyFingerprint(key: string): { text: string; spoken: string } | null {
  const k = key.trim()
  if (k === '') return null
  const n = [...k].length
  if (n <= KEY_TAIL * 2) {
    // Too short to show a tail without showing most of the key.
    return { text: `${n} characters`, spoken: `${n} characters` }
  }
  const tail = k.slice(-KEY_TAIL)
  return {
    text: `${n} characters, ending ···${tail}`,
    spoken: `${n} characters, ending ${tail}`,
  }
}

// ---------------------------------------------------------------------------
// Installing
// ---------------------------------------------------------------------------

/**
 * How long a key or filter change may take to reach the gateway before the UI
 * stops calling it normal. The api stamps `installing_until` this far ahead;
 * this constant only has to agree with it well enough to draw a bar, and the
 * bar is derived from the timestamp rather than from a local clock start, so
 * it survives a reload.
 *
 * Measured on kind, a changed Secret reaches the gateway in 80-90 seconds —
 * which is why the copy says "about two minutes" and this window is five. No
 * number is promised for any other cluster.
 */
export const INSTALL_WINDOW_MS = 5 * 60 * 1000

/** Past this much beyond `installing_until`, a stale stamp stops being news:
 *  the card goes back to reporting whatever the vendor's own state is. */
export const INSTALL_STALE_AFTER_MS = 60 * 60 * 1000

export type InstallPhase = 'installing' | 'taking_longer' | 'removing' | 'settled'

/** Provider states that are an answer about the key rather than the absence of
 *  one. Once the window has passed, these outrank "taking longer": the gateway
 *  has read the key and has something to say about it. */
const CONCLUSIVE: ReadonlySet<ProviderState> = new Set<ProviderState>([
  'ok',
  'auth_failed',
  'filter_missing',
  'filter_matched_nothing',
])

/**
 * Whether this vendor is still mid-install, or mid-removal.
 *
 * Inside the window, anything short of a working vendor reads as installing —
 * including `auth_failed`, which during a rotation is the gateway still
 * answering with the PREVIOUS key. Showing "Key rejected" over a key that has
 * not been read yet is the one wrong answer here.
 *
 * `ok` ends the wait immediately, whatever the stamp says: the change landed.
 *
 * Removing outranks both, and outranks the vendor's own state.
 *
 * The api deletes the key and stamps `removing_until`; the gateway is still
 * answering with it for a minute or two, so until then the vendor reports
 * itself configured and `ok`. Read literally that is "Connected", with Change
 * models, Replace key and Remove key beside it — a card describing a key that
 * has been deleted. The stamp is the later instruction and wins over the sync
 * that predates it, until the vendor reports the key gone or the stamp runs
 * out (a gateway that never syncs must not leave the card saying "Removing"
 * forever, so the window is the limit either way).
 */
export function installPhase(
  p: Pick<ProviderStatus, 'state' | 'installing_until' | 'removing_until' | 'configured'>,
  now: number,
): InstallPhase {
  const removing = parseTime(p.removing_until)
  // "Still there" is either signal, not both: whichever of the two the sync
  // has not caught up on yet is the one that would settle the card early.
  const stillThere = p.configured === true || p.state !== 'not_configured'
  if (removing !== null && now < removing && stillThere) return 'removing'

  const until = parseTime(p.installing_until)
  if (until === null) return 'settled'
  if (p.state === 'ok') return 'settled'
  if (now < until) return 'installing'
  if (now > until + INSTALL_STALE_AFTER_MS) return 'settled'
  return CONCLUSIVE.has(p.state) ? 'settled' : 'taking_longer'
}

/**
 * How far through the window this install is, 0 to 1.
 *
 * Derived from `installing_until` alone — the start is that moment less the
 * window — so a reload mid-install picks the bar up where it was instead of
 * starting it again. Determinate for the same reason: there is a real deadline
 * to count towards, and an indeterminate spinner on a two-minute wait reads as
 * a page that has hung.
 */
export function installProgress(installingUntil: string | null, now: number): number {
  const until = parseTime(installingUntil)
  if (until === null) return 1
  const elapsed = now - (until - INSTALL_WINDOW_MS)
  if (!Number.isFinite(elapsed)) return 1
  return Math.min(1, Math.max(0, elapsed / INSTALL_WINDOW_MS))
}

// ---------------------------------------------------------------------------
// Removing
// ---------------------------------------------------------------------------

/**
 * Whether a removal left this vendor's models switched on.
 *
 * The api's DELETE answers with `warnings`, and one of them is the difference
 * between two opposite facts on the card: models already off and quietly out of
 * the catalogue, or models still on and about to start refusing requests by
 * name. The warnings themselves are shown verbatim — this only decides which
 * sentence goes above them, and any warning this bundle does not recognise
 * leaves the safer of the two on screen.
 */
export function removalLeftModelsEnabled(warnings: readonly string[]): boolean {
  return warnings.some((w) => {
    const text = w.toLowerCase()
    return text.includes('model') && text.includes('enabl')
  })
}

/** What is true while a removal is on its way to the gateway. */
export function removingDetail(warnings: readonly string[]): string {
  return removalLeftModelsEnabled(warnings)
    ? 'The gateway stops using this key within about two minutes. Its models are still enabled and will start failing.'
    : 'The gateway stops using this key within about two minutes. Its models are already off.'
}

function parseTime(raw: string | null | undefined): number | null {
  if (!raw) return null
  const t = Date.parse(raw)
  return Number.isNaN(t) ? null : t
}

// ---------------------------------------------------------------------------
// What the gateway behind this api can do
// ---------------------------------------------------------------------------

/** The feature that has to be present before any of this is offered. Without
 *  it the install routes answer 409, so the card shows the command-line path
 *  instead — not a form that fails on submit. */
export const UI_KEY_INSTALL = 'ui_key_install'

export function supportsUiKeyInstall(features: readonly string[] | undefined): boolean {
  return (features ?? []).includes(UI_KEY_INSTALL)
}

// ---------------------------------------------------------------------------
// The transport the key would travel over
// ---------------------------------------------------------------------------

/**
 * The api refuses a key sent over plain HTTP — 403 `insecure_transport` on both
 * the install and the verify — except from a loopback host, where there is no
 * network to read it off. It reports the answer up front as
 * `key_install_transport_ok` on the models response, so the UI can decline to
 * ask for a key it cannot deliver.
 *
 * Absent means an api that predates the field, and then the browser's own
 * judgement stands in: `isSecureContext` is false on exactly the pages where
 * a key would cross the network in the clear, and true on localhost, which is
 * the same exception the api makes. A false negative here costs a form that
 * would have worked; the opposite costs a key typed into a page that will hand
 * it to whoever is on the path.
 */
export function canInstallKeyOverTransport(
  flag: boolean | null | undefined,
  secureContext: boolean,
): boolean {
  if (typeof flag === 'boolean') return flag
  return secureContext
}

/**
 * The fastest way to a transport the api will accept.
 *
 * The api's rule is `secureCookies || isLoopbackHost(request.Host)` (api
 * handler/providers.go keyInstallTransportOK), and nginx forwards the
 * browser's own Host to it (`proxy_set_header Host $host`). So a port-forward
 * makes the Host `localhost:8081`, the api takes the loopback exception, and
 * the key never leaves the machine — no certificate, no DNS, no redeploy.
 * That is the remedy worth naming first; TLS is the one worth doing, and it
 * is not the one to leave somebody stuck behind at 2am.
 */
export const WEB_PORT_FORWARD_COMMAND =
  'kubectl port-forward -n darkplum-system svc/darkplum-web 8081:80'

/** Where that port-forward puts Darkplum. Named in the copy so nobody has to
 *  infer the scheme from the port mapping. */
export const WEB_PORT_FORWARD_URL = 'http://localhost:8081'

/**
 * Why the key form is not on screen, and what to do instead.
 *
 * Two different faults end at the same refusal, and only one of these two
 * sentences is true of either. Saying "this page was served over plain HTTP"
 * on a page that is demonstrably HTTPS sends an administrator to check the
 * wrong thing — the api's answer comes from its own SECURE_COOKIES setting,
 * not from the scheme in the address bar.
 */
export const INSECURE_TRANSPORT_HELP =
  'Installing a key from here needs HTTPS. Darkplum is not served over HTTPS here, so a key typed into this page would cross the network readable, and Darkplum refuses it. Reach Darkplum over a port-forward instead — run the first command below, then open http://localhost:8081 and install the key there — or serve it over HTTPS (make expose-web CERT_ARN=…), or install the key with the second command.'

/** The same refusal from a page that IS HTTPS: the api is the half that is
 *  not configured for it, so the remedy is on the api, not on this page. The
 *  port-forward works here too, and for the same reason — the api takes the
 *  loopback exception whatever SECURE_COOKIES says. */
export const API_NOT_HTTPS_HELP =
  "This page is HTTPS, but Darkplum's api is not configured as HTTPS-served, so it refuses a key. Re-run make expose-web CERT_ARN=…, or set SECURE_COOKIES=true in the darkplum-auth Secret and restart the api. To install a key before either of those, reach Darkplum over a port-forward — run the first command below, then open http://localhost:8081 — or install it with the second command."

/** Which of the two is true of this page. */
export type KeyTransportHelp = 'page_not_https' | 'api_not_https'

export const KEY_TRANSPORT_HELP_TEXT: Record<KeyTransportHelp, string> = {
  page_not_https: INSECURE_TRANSPORT_HELP,
  api_not_https: API_NOT_HTTPS_HELP,
}

/** The api's loopback exception, in the browser's terms. `window.location
 *  .hostname` carries no port and brackets an IPv6 literal; the api strips
 *  both before comparing (handler/providers.go isLoopbackHost). */
function isLoopbackHostname(hostname: string): boolean {
  const h = hostname.replace(/^\[/, '').replace(/\]$/, '').toLowerCase()
  return h === 'localhost' || h === '127.0.0.1' || h === '::1'
}

/**
 * Which explanation this page has earned, or null when a key can be installed
 * from it and there is nothing to explain.
 *
 * `api_not_https` is claimed only where it is provable: the api said no
 * (`key_install_transport_ok === false`) from a page the browser calls secure
 * and a host that is not loopback. Every other refusal — a plain-HTTP page, a
 * loopback host the api declined anyway, an api too old to answer — gets the
 * page-side sentence, which names the port-forward and the command line and so
 * is never a dead end even when its diagnosis is the less precise of the two.
 */
export function keyTransportHelp(
  flag: boolean | null | undefined,
  secureContext: boolean,
  hostname: string,
): KeyTransportHelp | null {
  if (canInstallKeyOverTransport(flag, secureContext)) return null
  if (flag === false && secureContext && !isLoopbackHostname(hostname)) return 'api_not_https'
  return 'page_not_https'
}
