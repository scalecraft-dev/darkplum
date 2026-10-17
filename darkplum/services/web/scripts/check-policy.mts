/**
 * Residency truth table.
 *
 * Run: node --experimental-strip-types scripts/check-policy.mts
 *      (pnpm run check:policy). Imports the real module — no fixture, no
 *      second implementation to keep in step.
 *
 * residencyPermits() is the UI's copy of a rule the gateway enforces on every
 * request. A copy of a security rule is worth having only while it agrees with
 * the original, and it decides whether this product says "Runs in your
 * infrastructure" over a prompt that is on its way to a vendor's API.
 *
 * The table itself is NOT hand-typed here: every cell, and the ladder order,
 * comes from gateway/internal/policy/testdata/residency_truth_table.json —
 * the SAME golden the gateway's own policy.ResidencyPermits is asserted
 * against (see that package's residency_truth_table_test.go) and the api's
 * TestResidencyPermitsClass reads too (guilfoyle's M3 review finding: one
 * truth table, not three hand-typed copies that can silently drift apart).
 * Editing the ladder in the gateway without updating this file's copy of the
 * assertions is impossible — there IS no copy; this script fails to even
 * find cells to check if the golden goes missing.
 *
 *                   self_hosted  cloud_managed  external_api  unknown
 *   any                  yes          yes            yes         NO
 *   cloud_account_only   yes          yes            NO          NO
 *   self_hosted_only     yes          NO             NO          NO
 *
 * Two of those cells are the whole point. 'unknown' is refused under `any` —
 * a class nobody has reported cannot be shown to sit inside any boundary, so
 * the loosest rung is not a licence to guess. And a residency value this
 * bundle does not recognise is read as the strictest rung, so a newer server
 * can never widen an older UI's idea of what is permitted.
 *
 * Exits 1 with the failing cells named.
 */

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import {
  CLASS_RANK,
  DEFAULT_RESIDENCY,
  RESIDENCY_LADDER,
  RESIDENCY_RANK,
  orgPolicyRestricted,
  residencyPermits,
  residencyRank,
} from '../src/lib/residency.ts'
import { chatTagline, pickDefaultModel, type ChoosableModel } from '../src/lib/modelChoice.ts'
import {
  RETRY_AFTER_DEFAULT_SECONDS,
  RETRY_AFTER_MAX_SECONDS,
  failedVendorLabel,
  formatRetryCountdown,
  parseRetryAfter,
} from '../src/lib/modelRouting.ts'
import {
  API_NOT_HTTPS_HELP,
  INSECURE_TRANSPORT_HELP,
  INSTALL_WINDOW_MS,
  WEB_PORT_FORWARD_COMMAND,
  WEB_PORT_FORWARD_URL,
  admittedDiff,
  canInstallKeyOverTransport,
  escapeGlobLiteral,
  familyOf,
  globAdmits,
  globMatches,
  groupAdmittedModels,
  installPhase,
  installProgress,
  isBareWildcard,
  keyFingerprint,
  keyTransportHelp,
  removalLeftModelsEnabled,
  removingDetail,
  seedTickedModels,
  serialiseModelFilter,
  supportsUiKeyInstall,
  vendorKeyProblem,
} from '../src/lib/vendorKeys.ts'
import type { DisplayClass } from '../src/types/index.ts'

const failures: string[] = []

function check(what: string, actual: unknown, expected: unknown): void {
  if (actual !== expected) {
    failures.push(`${what}: expected ${String(expected)}, got ${String(actual)}`)
  }
}

// --- load the cross-service golden ------------------------------------------

const RESIDENCY_TRUTH_TABLE_PATH = fileURLToPath(
  new URL('../../gateway/internal/policy/testdata/residency_truth_table.json', import.meta.url),
)

interface ResidencyTruthTableCell {
  residency: string
  class: string
  permitted: boolean
}

interface ResidencyTruthTable {
  residency_rank: string[]
  known_classes: string[]
  unknown_token: string
  cells: ResidencyTruthTableCell[]
}

let table: ResidencyTruthTable
try {
  table = JSON.parse(readFileSync(RESIDENCY_TRUTH_TABLE_PATH, 'utf8')) as ResidencyTruthTable
} catch (err) {
  console.error(`Could not read the residency truth table at ${RESIDENCY_TRUTH_TABLE_PATH}: ${String(err)}`)
  console.error('Run `go test ./internal/policy/... -run TestResidencyTruthTable_MatchesGolden -update` in services/gateway to generate it.')
  process.exit(1)
}
if (table.cells.length === 0) {
  console.error(`${RESIDENCY_TRUTH_TABLE_PATH} has no cells — did the gateway generator run?`)
  process.exit(1)
}

// EXAMPLE_UNKNOWN_RESIDENCY is a concrete value NOT in table.residency_rank,
// exercising the cells whose residency is table.unknown_token — any other
// unrecognised string classifies identically (see residencyRank).
const EXAMPLE_UNKNOWN_RESIDENCY = 'from_the_future'

// --- residencyPermits, every cell from the golden ---------------------------

for (const cell of table.cells) {
  const residency = cell.residency === table.unknown_token ? EXAMPLE_UNKNOWN_RESIDENCY : cell.residency
  // The golden's unknown_token ("<unknown>") is this bundle's 'unknown'
  // DisplayClass literal — same concept, different spelling on each side of
  // the JSON boundary.
  const cls = (cell.class === table.unknown_token ? 'unknown' : cell.class) as DisplayClass
  check(`residencyPermits(${cell.residency}, ${cell.class})`, residencyPermits(residency, cls), cell.permitted)
}

// --- residencyRank matches the golden's ladder order, exactly --------------

for (let i = 0; i < table.residency_rank.length; i += 1) {
  check(`residencyRank(${table.residency_rank[i]})`, residencyRank(table.residency_rank[i]), i)
}
check(
  'residencyRank(<unknown rung>)',
  residencyRank(EXAMPLE_UNKNOWN_RESIDENCY),
  table.residency_rank.length - 1,
)
check('residencyRank(null)', residencyRank(null), RESIDENCY_RANK.self_hosted_only)
check('residencyRank(undefined)', residencyRank(undefined), RESIDENCY_RANK.self_hosted_only)

// --- the ladder is ordered loosest first, and holds every rung exactly once,
//     matching the golden's residency_rank both in length and in order ------

check('ladder length', RESIDENCY_LADDER.length, table.residency_rank.length)
check('ladder length', RESIDENCY_LADDER.length, Object.keys(RESIDENCY_RANK).length)
for (let i = 0; i < RESIDENCY_LADDER.length; i += 1) {
  check(`ladder[${i}] matches the golden's residency_rank order`, RESIDENCY_LADDER[i]?.value, table.residency_rank[i])
}
for (let i = 1; i < RESIDENCY_LADDER.length; i += 1) {
  const prev = RESIDENCY_LADDER[i - 1]
  const cur = RESIDENCY_LADDER[i]
  if (residencyRank(prev.value) >= residencyRank(cur.value)) {
    failures.push(`ladder order: ${prev.value} must come before a stricter rung, not ${cur.value}`)
  }
}

// --- "restricted" is a narrowing past the default, not the default itself ----
//
// Every install starts on the default rung. If that counted as restricted, the
// policy card and the catalogue would carry a warning in every organisation
// that has never touched them, which is the same as carrying it in none.

check(
  'orgPolicyRestricted(default rung)',
  orgPolicyRestricted({ residency: DEFAULT_RESIDENCY, allowed_models: null }),
  false,
)
check(
  'orgPolicyRestricted(loosest rung)',
  orgPolicyRestricted({ residency: 'any', allowed_models: null }),
  false,
)
check(
  'orgPolicyRestricted(strictest rung)',
  orgPolicyRestricted({ residency: 'self_hosted_only', allowed_models: null }),
  true,
)
check(
  'orgPolicyRestricted(allowlist on the loosest rung)',
  orgPolicyRestricted({ residency: 'any', allowed_models: ['a'] }),
  true,
)
check('orgPolicyRestricted(null)', orgPolicyRestricted(null), false)

// --- CLASS_RANK orders classes by containment, read off the same golden ------
//
// Not typed out here either: for any adjacent pair in CLASS_RANK order, the
// earlier class must be permitted by strictly more rungs than the later one.
// residencyPermits has already been checked cell-by-cell above, so by this
// point it IS the golden.

const RANKED_CLASSES = ([...table.known_classes, 'unknown'] as DisplayClass[]).sort(
  (a, b) => CLASS_RANK[a] - CLASS_RANK[b],
)
const rungsPermitting = (cls: DisplayClass): number =>
  table.residency_rank.filter((r) => residencyPermits(r, cls)).length
for (let i = 1; i < RANKED_CLASSES.length; i += 1) {
  const closer = RANKED_CLASSES[i - 1]
  const further = RANKED_CLASSES[i]
  if (rungsPermitting(closer) <= rungsPermitting(further)) {
    failures.push(
      `CLASS_RANK: ${closer} ranks before ${further} but is not held more closely than it`,
    )
  }
}
// The one literal worth pinning: whatever else the ladder grows, the class that
// never leaves the cluster is the one an unconfigured install starts on.
check('CLASS_RANK puts self_hosted first', RANKED_CLASSES[0], 'self_hosted')

// --- which model chat opens on ----------------------------------------------
//
// With no default_model configured the picker used to take the first entry in
// the list, which is alphabetical — so a catalogue holding "Claude …" alongside
// a self-hosted model opened on the vendor, and the first message a new user
// sent left the cloud account before they had read the header. The fallback
// orders by CLASS_RANK above; names only break ties inside a class.

const selfHosted: ChoosableModel = { id: 'zephyr-7b', owned_by: 'vllm', backend_class: 'self_hosted' }
const bedrock: ChoosableModel = { id: 'nova-pro', owned_by: 'amazon', backend_class: 'cloud_managed' }
const vendor: ChoosableModel = { id: 'claude-429', owned_by: 'anthropic', backend_class: 'external_api' }
const unreported: ChoosableModel = { id: 'aardvark', owned_by: 'vllm' }
/** Its default route is self-hosted; one of its routes is not. */
const mixedAlias: ChoosableModel = {
  id: 'assistant',
  owned_by: 'darkplum',
  backend_class: 'self_hosted',
  alias_kind: 'routed',
  // Verbatim shape of a routed alias in the api's chat-facing GET /v1/models
  // (api routeSummary: route_name, description, backend_class). The picker once
  // read `target_backend_class` here — the admin endpoints' name — so it never
  // saw a route's class and called this alias "Runs in your infrastructure".
  routes: [
    { route_name: 'code', description: 'code questions', backend_class: 'self_hosted' },
    { route_name: 'reasoning', description: 'hard reasoning', backend_class: 'external_api' },
  ],
}

check(
  'pickDefaultModel(mixed catalogue) takes the self-hosted model, not the alphabetical first',
  pickDefaultModel([vendor, bedrock, selfHosted, unreported]),
  selfHosted.id,
)
check(
  'pickDefaultModel(no self-hosted) prefers the cloud-managed model over the vendor',
  pickDefaultModel([vendor, bedrock]),
  bedrock.id,
)
check(
  'pickDefaultModel(vendor models only) still picks one',
  pickDefaultModel([{ id: 'gpt-4o', owned_by: 'openai', backend_class: 'external_api' }, vendor]),
  vendor.id,
)
check(
  'pickDefaultModel(unreported class only) still picks one',
  pickDefaultModel([unreported]),
  unreported.id,
)
check(
  'pickDefaultModel ranks an unreported class behind a vendor one',
  pickDefaultModel([unreported, vendor]),
  vendor.id,
)
check(
  'pickDefaultModel breaks ties alphabetically inside a class',
  pickDefaultModel([
    { id: 'zephyr-7b', backend_class: 'self_hosted' },
    { id: 'llama-3-8b', backend_class: 'self_hosted' },
  ]),
  'llama-3-8b',
)
check(
  'pickDefaultModel honours an administrator default that is a vendor model',
  pickDefaultModel([vendor, bedrock, selfHosted], vendor.id),
  vendor.id,
)
check(
  'pickDefaultModel ignores a default that is no longer on offer',
  pickDefaultModel([vendor, selfHosted], 'retired-model'),
  selfHosted.id,
)
check('pickDefaultModel([]) selects nothing', pickDefaultModel([]), '')
check(
  'pickDefaultModel ranks a routed alias by its least contained route',
  pickDefaultModel([mixedAlias, bedrock]),
  bedrock.id,
)

// --- the empty state never claims a boundary the prompt is about to cross ----
//
// This line sits directly under the picker's own "Sent to Anthropic — leaves
// your cloud account". Saying "running in your infrastructure" over it is the
// one sentence this product must never print.

check(
  'chatTagline(self-hosted)',
  chatTagline(selfHosted),
  'Private AI, running in your infrastructure.',
)
check(
  'chatTagline(cloud-managed)',
  chatTagline(bedrock),
  'Private AI, running in your cloud account.',
)
check(
  'chatTagline(vendor) names the vendor and the crossing',
  chatTagline(vendor),
  'This model is called at Anthropic. Your message leaves your cloud account.',
)
check('chatTagline(unreported class)', chatTagline(unreported), 'Private AI for your organisation.')
check('chatTagline(routed, mixed)', chatTagline(mixedAlias), 'Private AI for your organisation.')
check('chatTagline(nothing selected)', chatTagline(null), 'Private AI for your organisation.')
for (const claimant of [bedrock, vendor, unreported, mixedAlias]) {
  if (chatTagline(claimant).includes('your infrastructure')) {
    failures.push(`chatTagline(${claimant.id}) claims "your infrastructure" for a ${
      claimant.backend_class ?? 'unreported'
    } model`)
  }
}

// --- how long chat waits after a vendor rate limit ---------------------------
//
// A 429 on the one organisation-wide key is the likeliest vendor failure there
// is, and the wrong answer to it is a UI that invites everyone to press Try
// again immediately. The interval comes from the vendor's own Retry-After,
// relayed by the gateway and the api; everything unusable about that header
// has to land somewhere sensible rather than on NaN, a negative timer, or a
// button that stays shut for an hour.

check('parseRetryAfter("7") takes the vendor at its word', parseRetryAfter('7'), 7)
check('parseRetryAfter(" 7 ") ignores surrounding space', parseRetryAfter(' 7 '), 7)
check('parseRetryAfter("7.2") rounds up, never down', parseRetryAfter('7.2'), 8)
check('parseRetryAfter(absent header)', parseRetryAfter(null), RETRY_AFTER_DEFAULT_SECONDS)
check('parseRetryAfter(undefined)', parseRetryAfter(undefined), RETRY_AFTER_DEFAULT_SECONDS)
check('parseRetryAfter("")', parseRetryAfter(''), RETRY_AFTER_DEFAULT_SECONDS)
// The HTTP-date form is legal and no vendor here sends it. Half-reading it
// would be worse than the default.
check(
  'parseRetryAfter(HTTP-date form)',
  parseRetryAfter('Wed, 21 Oct 2026 07:28:00 GMT'),
  RETRY_AFTER_DEFAULT_SECONDS,
)
check('parseRetryAfter(garbage)', parseRetryAfter('soon'), RETRY_AFTER_DEFAULT_SECONDS)
check('parseRetryAfter("-5")', parseRetryAfter('-5'), RETRY_AFTER_DEFAULT_SECONDS)
check('parseRetryAfter("0")', parseRetryAfter('0'), RETRY_AFTER_DEFAULT_SECONDS)
check('parseRetryAfter(huge) is clamped', parseRetryAfter('86400'), RETRY_AFTER_MAX_SECONDS)
check('parseRetryAfter(Infinity)', parseRetryAfter('Infinity'), RETRY_AFTER_DEFAULT_SECONDS)
if (parseRetryAfter('3600') > RETRY_AFTER_MAX_SECONDS) {
  failures.push('parseRetryAfter: no header value may outlast RETRY_AFTER_MAX_SECONDS')
}

check('formatRetryCountdown(7)', formatRetryCountdown(7), '7s')
check('formatRetryCountdown(59)', formatRetryCountdown(59), '59s')
check('formatRetryCountdown(60)', formatRetryCountdown(60), '1m 00s')
check('formatRetryCountdown(75)', formatRetryCountdown(75), '1m 15s')
check('formatRetryCountdown(0)', formatRetryCountdown(0), '0s')
check('formatRetryCountdown(negative) never shows a minus', formatRetryCountdown(-3), '0s')

// --- who a vendor failure names ----------------------------------------------
//
// These messages are about the organisation's one shared key, and only two
// vendors have one. Naming anything else — a local runtime, a routed alias's
// own owner — invents a party that has no key and no rate limit.

check('failedVendorLabel(anthropic)', failedVendorLabel('anthropic'), 'Anthropic')
check('failedVendorLabel(openai)', failedVendorLabel('openai'), 'OpenAI')
check('failedVendorLabel(OpenAI, cased)', failedVendorLabel('OpenAI'), 'OpenAI')
check('failedVendorLabel(vllm) names no vendor', failedVendorLabel('vllm'), 'The model vendor')
check('failedVendorLabel(bedrock) names no vendor', failedVendorLabel('bedrock'), 'The model vendor')
check('failedVendorLabel(alias owner)', failedVendorLabel('darkplum'), 'The model vendor')
check('failedVendorLabel(null)', failedVendorLabel(null), 'The model vendor')
check('failedVendorLabel(undefined)', failedVendorLabel(undefined), 'The model vendor')
check('failedVendorLabel("")', failedVendorLabel(''), 'The model vendor')

// --- installing a vendor key from the UI --------------------------------------
//
// The model filter these functions produce decides which of a vendor's models
// the organisation is billed for, and it is written without anyone seeing the
// pattern. That makes serialiseModelFilter the one function on this surface
// worth pinning cell by cell: a glob that admits one model nobody ticked is a
// recurring charge with no screen it appears on.

// --- globMatches: Go's path.Match, which is what the gateway filters with ----
//
// services/gateway/internal/routing/filter.go calls path.Match per id. If this
// port disagrees with it, every "does this glob admit exactly the ticked rows"
// answer below is decided against the wrong rule.

const GLOB_CASES: Array<[string, string, boolean]> = [
  ['claude-sonnet*', 'claude-sonnet-4-5-20250929', true],
  ['claude-sonnet*', 'claude-opus-4-1-20250805', false],
  ['gpt*', 'gpt-4o', true],
  ['gpt*', 'gpt', true],
  ['gpt-4o', 'gpt-4o', true],
  ['gpt-4o', 'gpt-4o-mini', false],
  ['*', 'anything', true],
  // '*' stops at a separator, exactly as path.Match does. An id carrying one
  // is the case a prefix comparison would get wrong.
  ['ft:*', 'ft:org/model', false],
  ['ft:*/*', 'ft:org/model', true],
  ['gpt-4?', 'gpt-4o', true],
  ['gpt-4?', 'gpt-4om', false],
  ['gpt-[34]*', 'gpt-4o', true],
  ['gpt-[34]*', 'gpt-5', false],
  ['gpt-[^34]*', 'gpt-5', true],
  // A metacharacter matched literally once escaped.
  ['a\\*b', 'a*b', true],
  ['a\\*b', 'axb', false],
  // Malformed patterns never match; they must not throw either.
  ['[', 'x', false],
  ['[a-', 'a', false],
  ['a\\', 'a', false],
]
for (const [pattern, name, expected] of GLOB_CASES) {
  check(`globMatches(${JSON.stringify(pattern)}, ${JSON.stringify(name)})`, globMatches(pattern, name), expected)
}

check(
  'globAdmits keeps the order of the id list, not the pattern list',
  globAdmits(['gpt-4o*', 'o3'], ['o3', 'gpt-4o', 'gpt-4o-mini', 'gpt-5']).join(','),
  'o3,gpt-4o,gpt-4o-mini',
)
check('globAdmits([]) admits nothing', globAdmits([], ['gpt-4o']).length, 0)

check('isBareWildcard(*)', isBareWildcard('*'), true)
check('isBareWildcard(**)', isBareWildcard('**'), true)
check('isBareWildcard(?*)', isBareWildcard('?*'), true)
check('isBareWildcard(gpt*)', isBareWildcard('gpt*'), false)
check('isBareWildcard("")', isBareWildcard(''), false)

check('escapeGlobLiteral leaves an ordinary id alone', escapeGlobLiteral('gpt-4o'), 'gpt-4o')
check('escapeGlobLiteral escapes a metacharacter', escapeGlobLiteral('a*b[c]'), 'a\\*b\\[c\\]')

// --- familyOf ----------------------------------------------------------------

check('familyOf(claude-sonnet-4-5-20250929)', familyOf('claude-sonnet-4-5-20250929'), 'claude-sonnet')
check('familyOf(claude-opus-4-1-20250805)', familyOf('claude-opus-4-1-20250805'), 'claude-opus')
check('familyOf(claude-3-5-haiku-20241022)', familyOf('claude-3-5-haiku-20241022'), 'claude')
check('familyOf(gpt-4o-mini)', familyOf('gpt-4o-mini'), 'gpt')
check('familyOf(o4-mini) has nothing to strip', familyOf('o4-mini'), 'o4-mini')
check('familyOf(o3)', familyOf('o3'), 'o3')

// --- serialiseModelFilter -----------------------------------------------------

const ANTHROPIC = [
  'claude-opus-4-1-20250805',
  'claude-sonnet-4-20250514',
  'claude-sonnet-4-5-20250929',
  'claude-haiku-4-5-20251001',
  'claude-3-7-sonnet-20250219',
  'claude-3-5-haiku-20241022',
]
const OPENAI = ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1', 'o3', 'o4-mini']

const filterOf = (reachable: string[], ticked: string[]): string =>
  serialiseModelFilter(reachable, new Set(ticked)).join(',')

check('serialiseModelFilter(nothing ticked)', filterOf(ANTHROPIC, []), '')

// A family of two, wholly ticked, and the glob admits neither more nor less.
check(
  'a wholly ticked family of two becomes one glob',
  filterOf(ANTHROPIC, ['claude-3-7-sonnet-20250219', 'claude-3-5-haiku-20241022']),
  'claude-3-7-sonnet-20250219,claude-3-5-haiku-20241022',
)
// ^ "claude*" would admit the other three families too, so it is refused and
//   the two ids are written out. This is the case a looser rule gets wrong.

check(
  'a family of one is written exactly, never widened into a glob',
  filterOf(ANTHROPIC, ['claude-opus-4-1-20250805']),
  'claude-opus-4-1-20250805',
)
check(
  'a family of two with one ticked is written exactly',
  filterOf(ANTHROPIC, ['claude-sonnet-4-5-20250929']),
  'claude-sonnet-4-5-20250929',
)
check(
  'a family of two with both ticked becomes a glob',
  filterOf(ANTHROPIC, ['claude-sonnet-4-20250514', 'claude-sonnet-4-5-20250929']),
  'claude-sonnet*',
)

check(
  'everything ticked: only the families a glob describes exactly become globs',
  filterOf(ANTHROPIC, ANTHROPIC),
  'claude-opus-4-1-20250805,claude-sonnet*,claude-haiku-4-5-20251001,claude-3-7-sonnet-20250219,claude-3-5-haiku-20241022',
)

check(
  'a partly ticked family is written out id by id',
  filterOf(OPENAI, ['gpt-4o', 'o3']),
  'gpt-4o,o3',
)
check(
  'a wholly ticked gpt family becomes a glob; the single-member ones do not',
  filterOf(OPENAI, ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1', 'o3']),
  'gpt*,o3',
)

// Whatever it wrote, it admits exactly what was ticked — asserted against the
// same matcher the gateway uses, over every subset of a small catalogue.
const SUBSET_SOURCE = ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1', 'o3', 'o4-mini']
for (let mask = 0; mask < 1 << SUBSET_SOURCE.length; mask += 1) {
  const ticked = SUBSET_SOURCE.filter((_, i) => (mask & (1 << i)) !== 0)
  const patterns = serialiseModelFilter(SUBSET_SOURCE, new Set(ticked))
  const admitted = globAdmits(patterns, SUBSET_SOURCE)
  if (admitted.join(',') !== SUBSET_SOURCE.filter((id) => ticked.includes(id)).join(',')) {
    failures.push(
      `serialiseModelFilter([${ticked.join(',')}]) wrote [${patterns.join(',')}], which admits [${admitted.join(',')}]`,
    )
  }
  if (patterns.some(isBareWildcard)) {
    failures.push(`serialiseModelFilter([${ticked.join(',')}]) wrote a bare wildcard: [${patterns.join(',')}]`)
  }
  if (ticked.length > 0 && patterns.length === 0) {
    failures.push(`serialiseModelFilter([${ticked.join(',')}]) wrote an empty filter, which offers nothing`)
  }
}

// An id that is itself a pattern is written as a literal, not as a glob.
check(
  'a metacharacter in an id is escaped rather than left to match',
  filterOf(['weird*id', 'other'], ['weird*id']),
  'weird\\*id',
)

// --- the checklist's headings --------------------------------------------------
//
// Presentation only, but the noise case is real: a vendor whose ids do not
// carry a version segment produced a heading per model.

const groupsOf = (ids: string[]): string =>
  groupAdmittedModels(ids)
    .map((g) => `${g.family ?? 'other'}[${g.ids.join(' ')}]`)
    .join(' | ')

// A realistic Anthropic list: two families of two, two of one. More real
// families than lone models, so every heading stays.
check(
  'groupAdmittedModels(realistic Anthropic list) keeps every family heading',
  groupsOf([
    'claude-opus-5',
    'claude-opus-4-8',
    'claude-sonnet-5',
    'claude-sonnet-4-6',
    'claude-haiku-4-5',
    'claude-fable-5-1',
  ]),
  'claude-opus[claude-opus-5 claude-opus-4-8] | claude-sonnet[claude-sonnet-5 claude-sonnet-4-6] | claude-haiku[claude-haiku-4-5] | claude-fable[claude-fable-5-1]',
)

// A realistic OpenAI list: one family of three and two lone models, which get
// one heading between them instead of one each.
check(
  'groupAdmittedModels(realistic OpenAI list) sweeps the lone models up',
  groupsOf(['gpt-4o', 'gpt-4o-mini', 'gpt-4.1', 'o3', 'o4-mini']),
  'gpt[gpt-4o gpt-4o-mini gpt-4.1] | other[o3 o4-mini]',
)

// The case that prompted this: ids with no version segment, each its own
// "family", read as four headings over one row each.
check(
  'groupAdmittedModels(ids with no version segment) is one group, not four',
  groupsOf(['claude-429', 'claude-notemp', 'claude-slow', 'not-a-claude']),
  'other[claude-429 claude-notemp claude-slow not-a-claude]',
)

// One lone model is better named than swept: "Other" describes nothing here.
check(
  'groupAdmittedModels(a single family of one) keeps its own name',
  groupsOf(['claude-sonnet-5']),
  'claude-sonnet[claude-sonnet-5]',
)
check(
  'groupAdmittedModels(one family of two, one of one) keeps both headings',
  groupsOf(['gpt-4o', 'gpt-4o-mini', 'o3']),
  'gpt[gpt-4o gpt-4o-mini] | o3[o3]',
)
check('groupAdmittedModels([])', groupsOf([]), '')
// Whatever it groups, it groups all of it exactly once.
for (const list of [
  ['claude-429', 'claude-notemp', 'claude-slow', 'not-a-claude'],
  ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1', 'o3', 'o4-mini'],
  ANTHROPIC,
]) {
  const flat = groupAdmittedModels(list).flatMap((g) => g.ids)
  if (flat.slice().sort().join(',') !== list.slice().sort().join(',')) {
    failures.push(`groupAdmittedModels([${list.join(',')}]) lost or duplicated a model`)
  }
}

// --- which models a checklist opens with ---------------------------------------
//
// Getting this wrong widens a filter nobody edited. The case that broke: a
// Replace seeded from the modal's own (empty) tick set, which ticked every
// model the new key could reach — a five-model selection came back as six.

const seed = (params: {
  admitted: string[]
  reachable?: string[]
  currentFilter?: string[]
  currentModelIds?: string[]
  previous?: string[]
}): string =>
  [
    ...seedTickedModels({
      admitted: params.admitted,
      reachable: params.reachable ?? params.admitted,
      currentFilter: params.currentFilter ?? [],
      currentModelIds: params.currentModelIds ?? [],
      previous: new Set(params.previous ?? []),
    }),
  ].join(',')

// Install: nothing is on offer yet, so everything is ticked and trimmed down.
check(
  'seedTickedModels(install) ticks everything',
  seed({ admitted: OPENAI }),
  OPENAI.join(','),
)

// Replace, with the filter written as ids: the selection is preserved exactly.
check(
  'seedTickedModels(replace, narrower current filter) keeps the selection',
  seed({
    admitted: OPENAI,
    currentFilter: ['gpt-4o', 'o3'],
    currentModelIds: ['gpt-4o', 'o3'],
  }),
  'gpt-4o,o3',
)

// Replace, after a model was unticked: its catalogue row outlives the filter
// (it stays listed, unavailable, until it ages out). The filter is the
// selection; the stale row must not re-tick what the administrator removed.
// Found live: a 5-model selection came back as 6 after a key replacement.
check(
  'seedTickedModels(replace, catalogue still holds an unticked model) trusts the filter',
  seed({
    admitted: OPENAI,
    currentFilter: ['gpt-4o', 'o3'],
    currentModelIds: ['gpt-4o', 'o3', 'gpt-4.1'],
  }),
  'gpt-4o,o3',
)
// No filter known at all (an install that predates the UI): the catalogue is
// the only evidence of what is on offer, so it is the fallback.
check(
  'seedTickedModels(replace, no filter known) falls back to the catalogue',
  seed({ admitted: OPENAI, currentModelIds: ['gpt-4o', 'o3'] }),
  'gpt-4o,o3',
)

// Replace, with the filter written as a family glob: "currently offered" is
// what the glob admits from the reachable list, not the glob's own text.
check(
  'seedTickedModels(replace, family glob in the filter) resolves the glob',
  seed({
    admitted: OPENAI,
    currentFilter: ['gpt*'],
    currentModelIds: ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1'],
  }),
  'gpt-4o,gpt-4o-mini,gpt-4.1',
)
// The same glob, with the catalogue not yet synced: the patterns alone are
// enough, because they are what the gateway is filtering with.
check(
  'seedTickedModels(family glob, catalogue not yet synced)',
  seed({ admitted: OPENAI, currentFilter: ['gpt*'] }),
  'gpt-4o,gpt-4o-mini,gpt-4.1',
)

// A current id the new key cannot reach: dropped, and the rest kept — never
// taken as a reason to tick everything.
check(
  'seedTickedModels(a current id the new key cannot reach) keeps the rest',
  seed({
    admitted: ['gpt-4o', 'o3'],
    reachable: ['gpt-4o', 'o3'],
    currentFilter: ['gpt-4o', 'gpt-4o-mini'],
    currentModelIds: ['gpt-4o', 'gpt-4o-mini'],
  }),
  'gpt-4o',
)
// None of it reachable: an empty checklist has no Install button, so the new
// key's own list stands in.
check(
  'seedTickedModels(nothing in common with the new key) falls back to all',
  seed({
    admitted: ['o3', 'o4-mini'],
    currentFilter: ['gpt-4o'],
    currentModelIds: ['gpt-4o'],
  }),
  'o3,o4-mini',
)
// A second check in the same session keeps the ticks, not the stored filter.
check(
  'seedTickedModels(second check) keeps what was ticked',
  seed({
    admitted: OPENAI,
    currentFilter: ['gpt*'],
    currentModelIds: ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1'],
    previous: ['o3'],
  }),
  'o3',
)
// Replace never widens on its own: whatever the baseline, the result is a
// subset of it unless the baseline reaches nothing at all.
for (let mask = 1; mask < 1 << OPENAI.length; mask += 1) {
  const current = OPENAI.filter((_, i) => (mask & (1 << i)) !== 0)
  const ticked = seedTickedModels({
    admitted: OPENAI,
    reachable: OPENAI,
    currentFilter: current,
    currentModelIds: current,
    previous: new Set<string>(),
  })
  if (ticked.size !== current.length || !current.every((id) => ticked.has(id))) {
    failures.push(
      `seedTickedModels(replace with [${current.join(',')}]) opened on [${[...ticked].join(',')}]`,
    )
  }
}

// --- the key entry line -------------------------------------------------------
//
// The key is never shown. This line is the whole of what an administrator can
// check a paste against, so the two facts on it have to be right.

const KEY_108 = `${'x'.repeat(104)}a7Bq`
check('keyFingerprint counts characters and shows four', keyFingerprint(KEY_108)?.text, '108 characters, ending ···a7Bq')
check('keyFingerprint has a spoken form without the leader dots', keyFingerprint(KEY_108)?.spoken, '108 characters, ending a7Bq')
check('keyFingerprint(empty) says nothing at all', keyFingerprint(''), null)
check('keyFingerprint(whitespace only)', keyFingerprint('   '), null)
check('keyFingerprint(short) shows no tail', keyFingerprint('abcd1234')?.text, '8 characters')
check('keyFingerprint ignores surrounding whitespace', keyFingerprint(`  ${KEY_108}\n`)?.text, '108 characters, ending ···a7Bq')
// Whatever it says, it says at most four characters of the key.
for (const sample of ['', 'abc', 'abcd1234', KEY_108, 'sk-ant-'.padEnd(60, 'Z')]) {
  const line = keyFingerprint(sample)
  if (line && sample.trim().length > 8 && line.text.includes(sample.trim().slice(0, 5))) {
    failures.push('keyFingerprint leaked the beginning of the key')
  }
}

check('vendorKeyProblem(good key)', vendorKeyProblem(KEY_108), null)
check('vendorKeyProblem(empty)', vendorKeyProblem(''), 'Paste the key first.')
if (!vendorKeyProblem('short')?.includes('shorter')) failures.push('vendorKeyProblem(short) must say it is too short')
if (!vendorKeyProblem(`${'x'.repeat(513)}`)?.includes('longer')) failures.push('vendorKeyProblem(long) must say it is too long')
if (!vendorKeyProblem(`${'x'.repeat(30)} ${'y'.repeat(30)}`)?.includes('space')) {
  failures.push('vendorKeyProblem(embedded space) must name the space')
}
if (!vendorKeyProblem(`"${'x'.repeat(30)}"`)?.includes('Quotation')) {
  failures.push('vendorKeyProblem(quoted) must name the quotation marks')
}
check('vendorKeyProblem trims a pasted newline rather than refusing it', vendorKeyProblem(`${KEY_108}\n`), null)
// No message may ever contain the key it is about.
for (const bad of ['short', `"${'sEcReT'.repeat(6)}"`, `${'sEcReT'.repeat(6)} x`]) {
  const message = vendorKeyProblem(bad)
  if (message && message.includes('sEcReT')) failures.push('vendorKeyProblem put the key in its own message')
}

// --- installing ---------------------------------------------------------------

const NOW = Date.parse('2026-09-19T12:00:00Z')
const at = (ms: number): string => new Date(NOW + ms).toISOString()
const provider = (
  state: string,
  until: string | null,
  extra: { removing_until?: string | null; configured?: boolean } = {},
) =>
  ({
    state,
    installing_until: until,
    removing_until: extra.removing_until ?? null,
    // A vendor with a state other than not_configured has a key, unless the
    // case below says otherwise.
    configured: extra.configured ?? state !== 'not_configured',
  }) as Parameters<typeof installPhase>[0]

check('installPhase(nothing in flight)', installPhase(provider('not_configured', null), NOW), 'settled')
check('installPhase(inside the window)', installPhase(provider('not_configured', at(60_000)), NOW), 'installing')
// The gateway answering with the key it has not replaced yet. Reporting that
// as "Key rejected" over a freshly rotated key is the wrong answer here.
check('installPhase(auth_failed inside the window) is still installing', installPhase(provider('auth_failed', at(60_000)), NOW), 'installing')
check('installPhase(ok inside the window) has landed', installPhase(provider('ok', at(60_000)), NOW), 'settled')
check('installPhase(window passed, still nothing)', installPhase(provider('not_configured', at(-1_000)), NOW), 'taking_longer')
check('installPhase(window passed, unreachable)', installPhase(provider('unreachable', at(-1_000)), NOW), 'taking_longer')
// Past the window the gateway has read the key and has a verdict; that verdict
// is the news, not the wait.
check('installPhase(window passed, key rejected)', installPhase(provider('auth_failed', at(-1_000)), NOW), 'settled')
check('installPhase(window passed, filter matched nothing)', installPhase(provider('filter_matched_nothing', at(-1_000)), NOW), 'settled')
check('installPhase(a stamp from hours ago stops being news)', installPhase(provider('not_configured', at(-2 * 60 * 60_000)), NOW), 'settled')
check('installPhase(unparseable stamp)', installPhase(provider('not_configured', 'whenever'), NOW), 'settled')

// --- removing ------------------------------------------------------------------
//
// The api deletes the key and stamps `removing_until`; the gateway keeps
// answering with it for a minute or two, so the vendor block still says `ok`
// and `configured`. Read in that order the card offers Change models, Replace
// key and Remove key over a key that is already gone.

check(
  'installPhase(key deleted, gateway a sync behind) is removing, not Connected',
  installPhase(provider('ok', null, { removing_until: at(60_000) }), NOW),
  'removing',
)
check(
  'installPhase(removing outranks an install stamp)',
  installPhase(provider('ok', at(60_000), { removing_until: at(60_000) }), NOW),
  'removing',
)
check(
  'installPhase(removing outranks an install that is taking longer)',
  installPhase(provider('unreachable', at(-1_000), { removing_until: at(60_000) }), NOW),
  'removing',
)
check(
  'installPhase(removing outranks auth_failed)',
  installPhase(provider('auth_failed', null, { removing_until: at(60_000) }), NOW),
  'removing',
)
// The gateway has caught up: the card settles on "No key installed", with its
// Install key button back.
check(
  'installPhase(the gateway reports the key gone) settles',
  installPhase(
    provider('not_configured', null, { removing_until: at(60_000), configured: false }),
    NOW,
  ),
  'settled',
)
// Either signal still saying the key is there keeps the removal on screen.
check(
  'installPhase(state caught up, still flagged configured) is removing',
  installPhase(
    provider('not_configured', null, { removing_until: at(60_000), configured: true }),
    NOW,
  ),
  'removing',
)
// A gateway that never syncs must not leave the card saying "Removing" for
// ever: the stamp is the limit either way.
check(
  'installPhase(the removal stamp has passed) settles',
  installPhase(provider('ok', null, { removing_until: at(-1_000) }), NOW),
  'settled',
)
check(
  'installPhase(a removal stamp from hours ago) settles',
  installPhase(provider('ok', null, { removing_until: at(-2 * 60 * 60_000) }), NOW),
  'settled',
)
check(
  'installPhase(unparseable removal stamp) settles',
  installPhase(provider('ok', null, { removing_until: 'whenever' }), NOW),
  'settled',
)
// And a removal stamp does not mask an install that came after it: the api
// clears it when a key is written, so a cleared stamp reads as an install.
check(
  'installPhase(removal stamp cleared by a new key) is installing again',
  installPhase(provider('auth_failed', at(60_000), { removing_until: null }), NOW),
  'installing',
)

// Which of the two opposite sentences the card shows, decided by the api's own
// warnings rather than by the UI's guess about what the removal did.
const MODELS_LEFT_ON =
  '3 models from this vendor are still enabled; requests naming them will start failing.'
const MIRROR_WARNING =
  'The ConfigMap mirror could not be updated; the gateway refresh was not triggered.'
check('removalLeftModelsEnabled([])', removalLeftModelsEnabled([]), false)
check('removalLeftModelsEnabled(models left on)', removalLeftModelsEnabled([MODELS_LEFT_ON]), true)
check(
  'removalLeftModelsEnabled(an unrelated warning)',
  removalLeftModelsEnabled([MIRROR_WARNING]),
  false,
)
check(
  'removalLeftModelsEnabled(mixed)',
  removalLeftModelsEnabled([MIRROR_WARNING, MODELS_LEFT_ON]),
  true,
)
if (!removingDetail([]).includes('already off')) {
  failures.push('removingDetail(no warnings) must say the models are already off')
}
if (!removingDetail([MODELS_LEFT_ON]).includes('still enabled')) {
  failures.push('removingDetail(models left on) must say they are still enabled')
}
for (const warnings of [[], [MIRROR_WARNING], [MODELS_LEFT_ON]]) {
  if (!removingDetail(warnings).includes('about two minutes')) {
    failures.push('removingDetail must always say how long the gateway takes')
  }
}

check('installProgress(nothing in flight)', installProgress(null, NOW), 1)
check('installProgress(just started)', installProgress(at(INSTALL_WINDOW_MS), NOW), 0)
check('installProgress(halfway)', installProgress(at(INSTALL_WINDOW_MS / 2), NOW), 0.5)
check('installProgress(window passed) never goes past full', installProgress(at(-60_000), NOW), 1)
check('installProgress(clock skew) never goes below empty', installProgress(at(INSTALL_WINDOW_MS * 2), NOW), 0)
check('the install window is five minutes', INSTALL_WINDOW_MS, 300_000)

// --- what a filter change did --------------------------------------------------

const diff = admittedDiff(['gpt-4o', 'gpt-4o-mini', 'o3'], ['gpt-4o', 'o3', 'o4-mini'])
check('admittedDiff added', diff.added.join(','), 'o4-mini')
check('admittedDiff removed', diff.removed.join(','), 'gpt-4o-mini')
check('admittedDiff kept', diff.kept, 2)
check('admittedDiff unchanged', diff.unchanged, false)
check('admittedDiff(no change)', admittedDiff(['a', 'b'], ['b', 'a']).unchanged, true)
check('admittedDiff(from nothing)', admittedDiff([], ['a']).added.join(','), 'a')

// --- the feature gate ----------------------------------------------------------
//
// Absent means an older gateway, where the install routes answer 409. The card
// offers the command line in that case rather than a form that fails on submit,
// so reading this wrongly is a dead end for whoever is setting the key up.

check('supportsUiKeyInstall(present)', supportsUiKeyInstall(['ui_key_install']), true)
check('supportsUiKeyInstall(absent)', supportsUiKeyInstall([]), false)
check('supportsUiKeyInstall(undefined)', supportsUiKeyInstall(undefined), false)

// --- the transport the key would travel over ------------------------------------
//
// The api answers 403 insecure_transport to a key sent over plain HTTP, off
// loopback. The card has to know that before it offers a field, and an api
// that predates the flag leaves the browser to answer.

check('canInstallKeyOverTransport(api says yes)', canInstallKeyOverTransport(true, false), true)
check('canInstallKeyOverTransport(api says no)', canInstallKeyOverTransport(false, true), false)
check(
  'canInstallKeyOverTransport(absent, secure page)',
  canInstallKeyOverTransport(null, true),
  true,
)
check(
  'canInstallKeyOverTransport(absent, insecure page)',
  canInstallKeyOverTransport(null, false),
  false,
)
check(
  'canInstallKeyOverTransport(undefined, insecure page)',
  canInstallKeyOverTransport(undefined, false),
  false,
)
if (!INSECURE_TRANSPORT_HELP.startsWith('Installing a key from here needs HTTPS')) {
  failures.push('INSECURE_TRANSPORT_HELP must lead with the api\'s own sentence')
}
if (!INSECURE_TRANSPORT_HELP.includes('command')) {
  failures.push('INSECURE_TRANSPORT_HELP must point at the command-line path')
}

// --- which of the two transport faults it is ---------------------------------
//
// The api's rule is `SECURE_COOKIES || loopback Host` (handler/providers.go
// keyInstallTransportOK), and nginx passes the browser's Host through. So an
// HTTPS page can still be refused — by an api that was never told it is
// HTTPS-served — and "this page was served over plain HTTP" is then a false
// statement that sends an administrator to reissue a certificate they already
// have. Each message is claimed only where it is true.

check(
  'keyTransportHelp(plain HTTP page, api says no)',
  keyTransportHelp(false, false, 'darkplum.example.com'),
  'page_not_https',
)
check(
  'keyTransportHelp(HTTPS page, api says no) blames the api, not the page',
  keyTransportHelp(false, true, 'darkplum.example.com'),
  'api_not_https',
)
// Loopback is the api's own exception, so an api refusing one is not telling
// us which half is misconfigured. The page-side sentence names the
// port-forward and the command line and is therefore never a dead end.
for (const host of ['localhost', '127.0.0.1', '::1', '[::1]', 'LOCALHOST']) {
  check(
    `keyTransportHelp(${host}, api says no)`,
    keyTransportHelp(false, true, host),
    'page_not_https',
  )
}
check('keyTransportHelp(api says yes) explains nothing', keyTransportHelp(true, false, 'x'), null)
check(
  'keyTransportHelp(flag absent, secure page) explains nothing',
  keyTransportHelp(null, true, 'darkplum.example.com'),
  null,
)
check(
  'keyTransportHelp(flag absent, insecure page)',
  keyTransportHelp(undefined, false, 'darkplum.example.com'),
  'page_not_https',
)
// Whatever it answers, it answers exactly when a key cannot be installed.
for (const flag of [true, false, null, undefined]) {
  for (const secure of [true, false]) {
    for (const host of ['localhost', 'darkplum.example.com']) {
      const allowed = canInstallKeyOverTransport(flag, secure)
      if (allowed !== (keyTransportHelp(flag, secure, host) === null)) {
        failures.push(
          `keyTransportHelp(${String(flag)}, ${secure}, ${host}) disagrees with canInstallKeyOverTransport`,
        )
      }
    }
  }
}

// Both messages have to survive being read by somebody in a hurry at 2am: the
// ten-second remedy named before the one that needs a certificate, and the
// command line still there as the last resort.
for (const [name, message] of [
  ['INSECURE_TRANSPORT_HELP', INSECURE_TRANSPORT_HELP],
  ['API_NOT_HTTPS_HELP', API_NOT_HTTPS_HELP],
] as const) {
  if (!message.includes('port-forward')) {
    failures.push(`${name} must name the port-forward, which is the remedy that takes ten seconds`)
  }
  if (!message.includes(WEB_PORT_FORWARD_URL)) {
    failures.push(`${name} must say where the port-forward puts Darkplum`)
  }
  if (!message.includes('command')) {
    failures.push(`${name} must point at the command-line path`)
  }
}
// On a plain-HTTP page there is nothing to diagnose and nothing to weigh up:
// the port-forward is both the fastest way through and the only one that does
// not involve a certificate, so it goes first. (In the api-misconfigured case
// it does not: there the certificate IS the fix, and the port-forward is the
// way to get a key in before it lands.)
if (INSECURE_TRANSPORT_HELP.indexOf('port-forward') > INSECURE_TRANSPORT_HELP.indexOf('CERT_ARN')) {
  failures.push('INSECURE_TRANSPORT_HELP names the certificate before the port-forward')
}
if (!API_NOT_HTTPS_HELP.startsWith('This page is HTTPS')) {
  failures.push('API_NOT_HTTPS_HELP must start from what the administrator can see is true')
}
if (!API_NOT_HTTPS_HELP.includes('SECURE_COOKIES')) {
  failures.push('API_NOT_HTTPS_HELP must name the setting that actually decides the api\'s answer')
}
// The port the command opens is the port the copy sends them to. Getting this
// pair out of step sends an administrator to a page that is not there, in the
// one state where they have already been told twice that something is wrong.
const forwardedPort = new URL(WEB_PORT_FORWARD_URL).port
if (!WEB_PORT_FORWARD_COMMAND.endsWith(`${forwardedPort}:80`)) {
  failures.push(
    `WEB_PORT_FORWARD_COMMAND forwards a different port from the one ${WEB_PORT_FORWARD_URL} opens`,
  )
}
if (!WEB_PORT_FORWARD_COMMAND.startsWith('kubectl port-forward -n darkplum-system svc/darkplum-web')) {
  failures.push('WEB_PORT_FORWARD_COMMAND must match the command the Makefile prints')
}

// --- report ------------------------------------------------------------------

if (failures.length > 0) {
  console.error('Residency truth table does not hold:\n')
  for (const f of failures) console.error(`  ${f}`)
  console.error(
    '\nThis table mirrors the gateway\'s ResidencyPermits. If the gateway changed, change both.',
  )
  process.exit(1)
}

console.log(
  `Residency truth table holds (${table.cells.length} cells from ${RESIDENCY_TRUTH_TABLE_PATH} + ladder order + restricted rule + class rank, model fallback, empty-state claim, vendor rate-limit wait, who a vendor failure names, and the vendor-key helpers: glob matching, filter serialisation over every subset of a catalogue, the checklist's headings, what a Replace opens on, the key entry line, the installing and removing windows, the transport gate, which of the two transport faults each refusal names, and the admitted diff).`,
)
