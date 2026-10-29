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
  DENIAL_CODES,
  RETRY_AFTER_DEFAULT_SECONDS,
  RETRY_AFTER_MAX_SECONDS,
  aliasReferenceConsequence,
  aliasReferencesTo,
  denialLabel,
  classificationFailureWarning,
  refusalCause,
  refusalSuffix,
  isRouteTargetRefusal,
  ON_ROUTE_UNAVAILABLE_DEFAULT,
  ROUTE_TARGET_UNAVAILABLE_CODE,
  MODEL_CLASS_DOWNGRADED_CODE,
  onRouteUnavailableOf,
  routeOutcome,
  routeUnavailableHint,
  routeFallbackOf,
  routerReasonText,
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
  vendorErrorCopy,
  vendorTakesWorkspaceId,
  WORKSPACE_REQUIRED_MESSAGE,
  workspaceIdProblem,
  workspaceIdToSend,
  workspaceRequiredCopy,
} from '../src/lib/vendorKeys.ts'
import { formatEventDetail } from '../src/lib/activityEvents.ts'
import {
  AWS_OPT_IN_LABEL,
  AWS_OPT_IN_PICKER_LINE,
  DATA_RETENTION_CODE,
  awsOptInCountText,
  awsOptInRouteWarning,
  awsOptInStatusHint,
  dataRetentionFailureCopy,
  needsAwsOptIn,
} from '../src/lib/dataRetention.ts'
import type { DisplayClass, ModelAlias, ModelAliasRoute } from '../src/types/index.ts'
import {
  SPEND_GROUPS,
  UNPRICED_HINT,
  cachedShare,
  centsToDollarString,
  classSpendPoint,
  classTimeline,
  csvCell,
  formatAxisDollars,
  formatCost,
  formatPerMtok,
  formatShare,
  parsePriceInput,
  seedNote,
  spendCsv,
  spendRowLabel,
  timeBuckets,
  tokenSplitText,
  unpricedCaption,
} from '../src/lib/spend.ts'
import { EVENT_LABELS } from '../src/lib/activityEvents.ts'
import {
  DROPPED_NAME_MAX,
  PARAMS_MODE_NOTE,
  PARAMS_MODE_OPTIONS,
  PARAMS_MODE_STRICT_CONFIRM,
  droppedParamsBadge,
  droppedParamsTitle,
  paramsModeChip,
  paramsModeOf,
} from '../src/lib/chatParams.ts'
import { eventLabel, eventVariant } from '../src/lib/activityEvents.ts'
import {
  BILLING_KEY_NAME,
  PULL_COOLDOWN_MS,
  RECONCILIATION_DELTA_NOTE,
  billingKeyExplanation,
  billingStateCopy,
  billingVerifyErrorCopy,
  formatDelta,
  lastPulledText,
  pullCooldownSeconds,
} from '../src/lib/billing.ts'

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

// --- vendor_error copy ---------------------------------------------------------
{
  const full = vendorErrorCopy(
    'Anthropic',
    'Anthropic answered HTTP 400 (invalid_request_error) to the key check. Nothing was stored.',
    400,
    'invalid_request_error',
  )
  if (!full.startsWith('Anthropic answered HTTP 400 (invalid_request_error) to the key check. Nothing was stored.')) {
    failures.push('vendorErrorCopy must lead with the server message verbatim')
  }
  if (!full.includes('not a network problem')) failures.push('vendorErrorCopy must say it is not a network problem')
  const bare = vendorErrorCopy('OpenAI', '', 429, 'Bad Type!')
  if (!bare.startsWith('OpenAI answered HTTP 429 (unknown) to the key check.')) {
    failures.push(`vendorErrorCopy fallback wrong: ${bare}`)
  }
  if (/runbook/i.test(full + bare)) failures.push('vendorErrorCopy must not point at the network runbook')
}

// --- the Anthropic workspace id ------------------------------------------------
// Anthropic refuses an org-level key unless every request names a workspace.
// Only Anthropic is asked; the id mirrors the api's ^[A-Za-z0-9_-]{1,64}$;
// empty is valid (it clears); absent is sent only when there is nothing to say.
{
  check('vendorTakesWorkspaceId(anthropic)', vendorTakesWorkspaceId('anthropic'), true)
  check('vendorTakesWorkspaceId(openai)', vendorTakesWorkspaceId('openai'), false)

  check('workspaceIdProblem(empty)', workspaceIdProblem(''), null)
  check('workspaceIdProblem(blank)', workspaceIdProblem('   '), null)
  check('workspaceIdProblem(wrkspc id)', workspaceIdProblem('wrkspc_01AbC-x9'), null)
  check('workspaceIdProblem(padded)', workspaceIdProblem('  wrkspc_01  '), null)
  check('workspaceIdProblem(64 chars)', workspaceIdProblem('a'.repeat(64)), null)
  for (const bad of ['a'.repeat(65), 'wrkspc 01', 'wrkspc.01', '"wrkspc_01"', 'wrkspc/01', 'wrkspç']) {
    if (workspaceIdProblem(bad) === null) failures.push(`workspaceIdProblem(${JSON.stringify(bad)}) must refuse it`)
  }

  check('workspaceIdToSend(openai, value)', workspaceIdToSend('openai', 'wrkspc_1', null), undefined)
  check('workspaceIdToSend(anthropic, empty over none)', workspaceIdToSend('anthropic', '', null), undefined)
  check('workspaceIdToSend(anthropic, empty over stored) clears', workspaceIdToSend('anthropic', ' ', 'wrkspc_1'), '')
  check('workspaceIdToSend(anthropic, value) trimmed', workspaceIdToSend('anthropic', ' wrkspc_2 ', null), 'wrkspc_2')
  check('workspaceIdToSend(anthropic, unchanged) still sent', workspaceIdToSend('anthropic', 'wrkspc_1', 'wrkspc_1'), 'wrkspc_1')

  const server =
    'This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace.'
  check('WORKSPACE_REQUIRED_MESSAGE matches the api', WORKSPACE_REQUIRED_MESSAGE, server)
  check(
    'workspaceRequiredCopy(server message)',
    workspaceRequiredCopy(server),
    `${server} Keys created inside a workspace don't need this.`,
  )
  check(
    'workspaceRequiredCopy(empty) falls back to the api sentence',
    workspaceRequiredCopy(''),
    `${server} Keys created inside a workspace don't need this.`,
  )

  check(
    'activity labels for workspace_id_before/after',
    formatEventDetail({ workspace_id_before: 'wrkspc_a', workspace_id_after: 'wrkspc_b' }),
    'workspace before: wrkspc_a, workspace after: wrkspc_b',
  )
}

// --- Bedrock data-retention opt-in -------------------------------------------
// Every sentence the chat card, the Models row, the picker and the alias
// editor say about it, pinned word for word, plus the precedence inputs.
{
  check('needsAwsOptIn(needs_aws_opt_in)', needsAwsOptIn({ availability: { state: 'needs_aws_opt_in' } }), true)
  check('needsAwsOptIn(ok)', needsAwsOptIn({ availability: { state: 'ok' } }), false)
  check('needsAwsOptIn(unknown) says nothing', needsAwsOptIn({ availability: { state: 'unknown' } }), false)
  check('needsAwsOptIn(null)', needsAwsOptIn({ availability: null }), false)
  check('needsAwsOptIn(absent, older api / non-Bedrock)', needsAwsOptIn({}), false)
  check('needsAwsOptIn(no model)', needsAwsOptIn(null), false)
  check('DATA_RETENTION_CODE matches the gateway', DATA_RETENTION_CODE, 'model_requires_data_retention')
  check('AWS_OPT_IN_LABEL', AWS_OPT_IN_LABEL, 'Needs AWS opt-in')
  check('denialLabel(model_requires_data_retention) is the badge phrase', denialLabel(DATA_RETENTION_CODE), AWS_OPT_IN_LABEL)
  check(
    'DENIAL_CODES offers model_requires_data_retention (api accepts the filter)',
    (DENIAL_CODES as readonly string[]).includes(DATA_RETENTION_CODE),
    true,
  )
  check(
    'awsOptInStatusHint',
    awsOptInStatusHint({ availability: { state: 'needs_aws_opt_in', account_mode: 'default', required_mode: 'aws_review' } }),
    "Bedrock refuses this model while your AWS account's data-retention mode is 'default'; it needs 'aws_review'. Raising it lets AWS retain and review prompts to models like this one for up to 30 days, inside AWS — the model provider does not receive them. Set it per region with the Bedrock data-retention API; see the runbook.",
  )
  check(
    'awsOptInStatusHint(no modes) never prints an empty quote',
    awsOptInStatusHint({ availability: { state: 'needs_aws_opt_in' } }).startsWith(
      "Bedrock refuses this model while your AWS account's data-retention mode is 'unknown'; it needs 'aws_review'.",
    ),
    true,
  )
  check('awsOptInCountText(1)', awsOptInCountText(1), '1 needs an AWS opt-in')
  check('awsOptInCountText(3)', awsOptInCountText(3), '3 need an AWS opt-in')
  check(
    'AWS_OPT_IN_PICKER_LINE',
    AWS_OPT_IN_PICKER_LINE,
    "Needs an AWS opt-in — an administrator has to raise the account's Bedrock data-retention mode",
  )
  check(
    'awsOptInRouteWarning',
    awsOptInRouteWarning('anthropic.claude-fable-5-1-v1:0'),
    'anthropic.claude-fable-5-1-v1:0 needs an AWS opt-in before it can answer.',
  )
  const direct = dataRetentionFailureCopy({ modelName: 'Claude Fable 5.1' })
  check('dataRetentionFailureCopy(direct).title', direct.title, 'Claude Fable 5.1 needs an AWS opt-in')
  check('dataRetentionFailureCopy(direct).hint is empty', direct.hint, '')
  const routed = dataRetentionFailureCopy({ modelName: 'Claude Fable 5.1', route: 'deep-analysis', alias: 'smart' })
  check('dataRetentionFailureCopy(routed).title', routed.title, 'Claude Fable 5.1 needs an AWS opt-in')
  check(
    'dataRetentionFailureCopy(routed).hint',
    routed.hint,
    'Your message was routed to it via the deep-analysis route of smart.',
  )
}

// --- a routed alias whose picked route is down -------------------------------
// What happens is the alias's on_route_unavailable setting applied to the two
// classes (the gateway's resolveRoutedPick): refuse never uses another route;
// fallback_same_or_stricter (the default) uses the default route only when its
// class is the same as or stricter than the picked route's; fallback_any_permitted
// uses it whenever it can answer. Unknown or unrecorded classes refuse. A
// classifier failure is a different case and always uses the default route.

{
  check('default setting', ON_ROUTE_UNAVAILABLE_DEFAULT, 'fallback_same_or_stricter')
  check('onRouteUnavailableOf(absent) is the default', onRouteUnavailableOf({}), 'fallback_same_or_stricter')
  check('onRouteUnavailableOf(null) is the default', onRouteUnavailableOf({ on_route_unavailable: null }), 'fallback_same_or_stricter')
  check('onRouteUnavailableOf(refuse)', onRouteUnavailableOf({ on_route_unavailable: 'refuse' }), 'refuse')
  check('onRouteUnavailableOf(<future value>) fails closed', onRouteUnavailableOf({ on_route_unavailable: 'whatever' }), 'refuse')

  // historical and current reason text (a) -----------------------------------
  check(
    'routerReasonText(fallback disabled)',
    routerReasonText('fallback:route_target_disabled:code'),
    "The default route answered instead — the model on route 'code' was disabled at the time",
  )
  check(
    'routerReasonText(fallback unreachable)',
    routerReasonText('fallback:route_target_unreachable:code'),
    "The default route answered instead — the model on route 'code' was not reachable at the time",
  )
  check('routeFallbackOf(timeout) is null', routeFallbackOf('fallback:timeout'), null)
  check('routeFallbackOf(disabled).route', routeFallbackOf('fallback:route_target_disabled:code')?.route, 'code')
  check('ROUTE_TARGET_UNAVAILABLE_CODE matches the gateway', ROUTE_TARGET_UNAVAILABLE_CODE, 'route_target_unavailable')
  check('MODEL_CLASS_DOWNGRADED_CODE matches the gateway', MODEL_CLASS_DOWNGRADED_CODE, 'model_class_downgraded')
  check('denialLabel(model_class_downgraded)', denialLabel(MODEL_CLASS_DOWNGRADED_CODE), 'Usual backend unavailable')
  check(
    'DENIAL_CODES offers model_class_downgraded (api accepts the filter)',
    (DENIAL_CODES as readonly string[]).includes(MODEL_CLASS_DOWNGRADED_CODE),
    true,
  )
  // It is a model-level refusal, not a routed pick: chat must not send an
  // admin to Routing for it.
  check('isRouteTargetRefusal(model_class_downgraded) is false', isRouteTargetRefusal(MODEL_CLASS_DOWNGRADED_CODE, 'model "m" normally runs in your own cluster'), false)

  // routeOutcome: every setting x class relation -----------------------------
  type C = 'self_hosted' | 'cloud_managed' | 'external_api' | 'unknown'
  const def = (cls: C, usable = true) => ({ name: 'general', class: cls, usable })
  const kindOf = (setting: Parameters<typeof routeOutcome>[0], pick: C, d: ReturnType<typeof def> | null) =>
    routeOutcome(setting, pick, d).kind
  const matrix: [string, C, ReturnType<typeof def> | null, string, string, string][] = [
    // name, pick class, default, refuse, same_or_stricter, any_permitted
    ['same class', 'self_hosted', def('self_hosted'), 'refused', 'fallback', 'fallback'],
    ['stricter default', 'cloud_managed', def('self_hosted'), 'refused', 'fallback', 'fallback'],
    ['looser default (issue 13)', 'self_hosted', def('external_api'), 'refused', 'refused', 'fallback'],
    ['one rung looser', 'self_hosted', def('cloud_managed'), 'refused', 'refused', 'fallback'],
    ['unknown default class (refused by residency after the pick)', 'self_hosted', def('unknown'), 'refused', 'refused', 'refused'],
    ['unknown picked class', 'unknown', def('self_hosted'), 'refused', 'refused', 'fallback'],
    ['default unusable', 'self_hosted', def('self_hosted', false), 'refused', 'refused', 'refused'],
    ['no default', 'self_hosted', null, 'refused', 'refused', 'refused'],
  ]
  for (const [name, pick, d, refuse, same, any] of matrix) {
    check(`outcome refuse / ${name}`, kindOf('refuse', pick, d), refuse)
    check(`outcome same_or_stricter / ${name}`, kindOf('fallback_same_or_stricter', pick, d), same)
    check(`outcome any_permitted / ${name}`, kindOf('fallback_any_permitted', pick, d), any)
  }

  // hints (b), (c) and the fallback wording ------------------------------------
  const refuseOutcome = routeOutcome('refuse', 'self_hosted', def('self_hosted'))
  const looserOutcome = routeOutcome('fallback_same_or_stricter', 'self_hosted', def('external_api'))
  const anyOutcome = routeOutcome('fallback_any_permitted', 'self_hosted', def('external_api'))
  const sameOutcome = routeOutcome('fallback_same_or_stricter', 'self_hosted', def('self_hosted'))
  check(
    'hint(unreachable, refuse)',
    routeUnavailableHint('unreachable', refuseOutcome),
    'This model is not reachable right now, so messages that take this route are refused until the backend is back. They are never sent to another model. This alias is set to refuse rather than use another route.',
  )
  check('hint(disabled, refused) suffix', routeUnavailableHint('disabled', refuseOutcome).includes('are refused until it is fixed.'), true)
  check('hint(missing, refused) suffix', routeUnavailableHint('missing', refuseOutcome).includes('are refused until it is fixed.'), true)
  check('hint(unreachable) never says "until it is fixed"', routeUnavailableHint('unreachable', looserOutcome).includes('until it is fixed'), false)
  check(
    'hint(disabled, looser default) names both classes',
    routeUnavailableHint('disabled', looserOutcome),
    'This model is turned off, so messages that take this route are refused until it is fixed. They are never sent to another model. This alias only falls back to the same or a stricter class, and the default route general (outside your cloud account) is looser than this route (in your own cluster).',
  )
  check(
    'hint(disabled, same class) falls back',
    routeUnavailableHint('disabled', sameOutcome),
    'This model is turned off, so messages that take this route are answered on the default route general (in your own cluster) until it is fixed.',
  )
  check(
    'hint(any_permitted, looser default) says where prompts go',
    routeUnavailableHint('disabled', anyOutcome),
    'This model is turned off, so messages that take this route are answered on the default route general (outside your cloud account) until it is fixed. That is looser than this route (in your own cluster); this alias\'s setting allows it.',
  )
  check(
    'hint(default route) adds the classification-failure sentence',
    routeUnavailableHint('unreachable', null),
    'This model is not reachable right now, so messages that take this route are refused until the backend is back. They are never sent to another model. It is also the default route, so messages are refused when classification fails.',
  )
  check('hint(refused, no default) names it', routeUnavailableHint('disabled', routeOutcome('fallback_any_permitted', 'self_hosted', def('self_hosted', false))).includes('The default route general cannot answer either.'), true)

  // The gateway's test for the default route is "in the route table and
  // enabled" -- not recent discovery. A default whose target_available is
  // false must therefore still count as usable, or the UI prints a refusal
  // ("never sent to another model") the gateway does not make.
  {
    const rt = (name: string, target: string, avail: boolean | undefined, cls: C = 'self_hosted'): ModelAliasRoute => ({
      route_name: name, description: '', target_model_id: target, keywords: [],
      target_exists: true, target_enabled: true, target_available: avail, target_backend_class: cls,
    })
    const a = (setting: ModelAlias['on_route_unavailable'], avail: boolean | undefined): ModelAlias => ({
      alias: 'auto', kind: 'routed', default_route: 'general', on_route_unavailable: setting,
      target_model_id: null, created_at: '', updated_at: '',
      routes: [rt('general', 'llama', avail), rt('code', 'fable', true)],
    } as ModelAlias)
    check('disable: default not recently discovered still takes over (same class)',
      aliasReferencesTo('fable', [a('fallback_same_or_stricter', false)])[0]?.outcome, 'fallback')
    check('disable: default not recently discovered still takes over (any_permitted)',
      aliasReferencesTo('fable', [a('fallback_any_permitted', false)])[0]?.outcome, 'fallback')
    check('disable: refuse is still refuse when the default is available',
      aliasReferencesTo('fable', [a('refuse', true)])[0]?.outcome, 'refused')
  }
  check('refusalSuffix(unknown)', refusalSuffix('unknown'), 'refused until a backend reports where the model runs')
  check('refusalSuffix(blocked)', refusalSuffix('blocked'), 'refused under the current policy')
  check('refusalCause(unknown default, any_permitted)',
    refusalCause(routeOutcome('fallback_any_permitted', 'self_hosted', def('unknown'))),
    'The default route general has not reported where it runs, and a model whose class is unknown is refused under every policy.')
  check('refusalCause(unknown picked, same_or_stricter)',
    refusalCause(routeOutcome('fallback_same_or_stricter', 'unknown', def('self_hosted'))),
    'This alias only falls back to a known same or stricter class, and the class of this route is not reported.')
  check('unknown picked class falls back under any_permitted (gateway: only the default is residency-checked)',
    kindOf('fallback_any_permitted', 'unknown', def('self_hosted')), 'fallback')

  // classification-failure warning in the editor -------------------------------
  check(
    'classificationFailureWarning(vendor default, local other)',
    classificationFailureWarning('general', 'external_api', ['self_hosted', 'external_api']),
    'If classification fails, messages go to general (outside your cloud account), whatever the setting below says. Make a stricter route the default if that is not acceptable.',
  )
  check('classificationFailureWarning(default is the strictest)', classificationFailureWarning('general', 'self_hosted', ['external_api']), null)
  check('classificationFailureWarning(equal classes)', classificationFailureWarning('general', 'cloud_managed', ['cloud_managed']), null)
  check('classificationFailureWarning(no other routes)', classificationFailureWarning('general', 'external_api', []), null)
  check(
    'classificationFailureWarning(unknown default)',
    classificationFailureWarning('general', 'unknown', ['self_hosted']),
    'If classification fails, messages go to general (where it runs is not known), whatever the setting below says. Make a stricter route the default if that is not acceptable.',
  )

  check(
    'denialLabel(route_target_unavailable)',
    denialLabel(ROUTE_TARGET_UNAVAILABLE_CODE),
    'Route target unavailable',
  )
  check(
    'DENIAL_CODES offers route_target_unavailable (api accepts the filter)',
    (DENIAL_CODES as readonly string[]).includes(ROUTE_TARGET_UNAVAILABLE_CODE),
    true,
  )
  // The 503 the gateway sends for a non-default route (chat.go resolveRoutedPick).
  check(
    'isRouteTargetRefusal(route_target_unavailable)',
    isRouteTargetRefusal(
      ROUTE_TARGET_UNAVAILABLE_CODE,
      `route "code" of alias "auto" selects model "a", which is disabled. The request was not sent anywhere else.`,
    ),
    true,
  )
  check('isRouteTargetRefusal(route_target_unavailable, empty message)', isRouteTargetRefusal(ROUTE_TARGET_UNAVAILABLE_CODE, ''), true)
  // The refusal the gateway keeps for a pick of the default route itself.
  check(
    'isRouteTargetRefusal(default route)',
    isRouteTargetRefusal(
      'model_not_found',
      `route "general" of alias "auto" is the alias's default route and selects model "b", which is not available on this gateway. Enable it on the Models page or retarget the route.`,
    ),
    true,
  )
  check(
    'isRouteTargetRefusal(plain model_not_enabled) is false',
    isRouteTargetRefusal('model_not_enabled', 'model "x" is not enabled'),
    false,
  )
  check(
    'isRouteTargetRefusal(residency) is false',
    isRouteTargetRefusal(
      'model_residency_violation',
      `route "code" of alias "auto" selects model "a", which is not enabled/allowed by residency`,
    ),
    false,
  )

  // disable-model confirmation: computed from the alias setting and classes ----
  const route = (name: string, target: string, cls: C = 'self_hosted', enabled = true): ModelAliasRoute => ({
    route_name: name,
    description: '',
    target_model_id: target,
    keywords: [],
    target_exists: true,
    target_enabled: enabled,
    target_backend_class: cls === 'unknown' ? null : cls,
  })
  const alias = (a: Partial<ModelAlias> & { alias: string }): ModelAlias => ({
    target_model_id: null,
    target_exists: true,
    target_enabled: true,
    target_backend_class: 'self_hosted',
    created_at: '',
    updated_at: '',
    ...a,
  })
  const routed = (name: string, setting: ModelAlias['on_route_unavailable'], general: ModelAliasRoute, other: ModelAliasRoute) =>
    alias({ alias: name, kind: 'routed', default_route: 'general', on_route_unavailable: setting, routes: [general, other] })
  const consequence = (aliases: ModelAlias[], model: string, index = 0) => {
    const r = aliasReferencesTo(model, aliases)[index]
    return r ? aliasReferenceConsequence(r) : undefined
  }

  // same-class default: falls back under the default and any_permitted, not refuse
  const sameClass = (setting: ModelAlias['on_route_unavailable']) =>
    [routed('auto', setting, route('general', 'llama'), route('code', 'fable'))]
  check(
    'disable: same class default, default setting',
    consequence(sameClass(undefined), 'fable'),
    'messages that take this route will use the default route general (in your own cluster) instead.',
  )
  check('disable: same class default, same_or_stricter', consequence(sameClass('fallback_same_or_stricter'), 'fable')?.includes('will use the default route general'), true)
  check('disable: same class default, any_permitted', consequence(sameClass('fallback_any_permitted'), 'fable')?.includes('will use the default route general'), true)
  check(
    'disable: same class default, refuse',
    consequence(sameClass('refuse'), 'fable'),
    'messages that take this route will be refused, never sent to another model, until it is retargeted or re-enabled. This alias is set to refuse rather than use another route.',
  )
  // looser default: only any_permitted moves the prompt, and it says where
  const looser = (setting: ModelAlias['on_route_unavailable']) =>
    [routed('auto', setting, route('general', 'gpt', 'external_api'), route('code', 'fable'))]
  check(
    'disable: looser default, default setting refuses and names both classes',
    consequence(looser(undefined), 'fable'),
    'messages that take this route will be refused, never sent to another model, until it is retargeted or re-enabled. This alias only falls back to the same or a stricter class, and the default route general (outside your cloud account) is looser than this route (in your own cluster).',
  )
  check(
    'disable: looser default, any_permitted says where prompts go',
    consequence(looser('fallback_any_permitted'), 'fable'),
    'messages that take this route will use the default route general (outside your cloud account) instead. That is looser than this route (in your own cluster); this alias\'s setting allows it.',
  )
  check('disable: looser default, outcome field', aliasReferencesTo('fable', looser(undefined))[0]?.outcome, 'refused')
  check('disable: looser default any_permitted, outcome field', aliasReferencesTo('fable', looser('fallback_any_permitted'))[0]?.outcome, 'fallback')
  // unknown class on the picked route (a model no backend has reported)
  check(
    'disable: unknown picked class refuses under the default setting',
    aliasReferencesTo('fable', [routed('auto', undefined, route('general', 'llama'), route('code', 'fable', 'unknown'))])[0]?.outcome,
    'refused',
  )
  // default cannot answer: disabled already, or the very model being turned off
  check(
    'disable: default already disabled refuses even under any_permitted',
    aliasReferencesTo('fable', [routed('auto', 'fallback_any_permitted', route('general', 'off', 'self_hosted', false), route('code', 'fable'))])[0]?.outcome,
    'refused',
  )
  check(
    'disable: default targets the same model refuses',
    aliasReferencesTo('fable', [routed('auto', 'fallback_any_permitted', route('general', 'fable'), route('code', 'fable'))]).map((r) => r.outcome).join(','),
    'refused,refused',
  )
  // residency, when the caller passes it, can make the default unusable
  check(
    'disable: default class the policy refuses cannot take over',
    aliasReferencesTo('fable', looser('fallback_any_permitted'), 'cloud_account_only')[0]?.outcome,
    'refused',
  )
  // the default route itself, and static aliases
  const statics = [alias({ alias: 'default-chat', kind: 'static', target_model_id: 'fable' })]
  check('disable: static alias', consequence(statics, 'fable'), 'refused until it is retargeted or the model is re-enabled')
  check(
    'disable: the default route itself adds the classification-failure sentence',
    consequence(sameClass(undefined), 'llama'),
    'messages that take this route will be refused until it is retargeted or re-enabled. It is also the default route, so messages are refused when classification fails.',
  )
  check('aliasReferencesTo(unused).length', aliasReferencesTo('nothing', sameClass(undefined)).length, 0)
}

// --- estimated spend: money, shares, labels, CSV ------------------------------
//
// Every amount on Activity > Spend is a list-price estimate. These pin the
// three ways that framing can quietly break: a missing price shown as $0.00
// (it must be null → "—"), a sub-cent cost rounded to $0.00 (it must keep up
// to four places), and a CSV that rounds what the screen merely displays.
{
  const same = (what: string, actual: unknown, expected: unknown) =>
    check(what, JSON.stringify(actual), JSON.stringify(expected))

  // cents → display
  check('formatCost(null) is unpriced, not $0', formatCost(null), null)
  check('formatCost(garbage) is unpriced', formatCost('abc'), null)
  check('formatCost("0")', formatCost('0'), '$0.00')
  check('formatCost("123456.7")', formatCost('123456.7'), '$1,234.57')
  check('formatCost("1")', formatCost('1'), '$0.01')
  check('formatCost sub-cent keeps 4dp', formatCost('0.42'), '$0.0042')
  check('formatCost sub-cent trims zeros', formatCost('0.5'), '$0.005')
  check('formatCost below 4dp', formatCost('0.001'), '<$0.0001')
  check('formatCost(number)', formatCost(250), '$2.50')

  // exact dollars for the CSV
  check('centsToDollarString("1234.5678")', centsToDollarString('1234.5678'), '12.345678')
  check('centsToDollarString("5")', centsToDollarString('5'), '0.05')
  check('centsToDollarString("0.0042")', centsToDollarString('0.0042'), '0.000042')
  check('centsToDollarString("100")', centsToDollarString('100'), '1')
  check('centsToDollarString("-250")', centsToDollarString('-250'), '-2.5')
  check('centsToDollarString("0")', centsToDollarString('0'), '0')
  check('centsToDollarString(null)', centsToDollarString(null), null)

  // $/MTok
  check('formatPerMtok("3")', formatPerMtok('3'), '$3.00')
  check('formatPerMtok("0.075")', formatPerMtok('0.075'), '$0.075')
  check('formatPerMtok("1.23456")', formatPerMtok('1.23456'), '$1.2346')
  check('formatPerMtok("15.0000")', formatPerMtok('15.0000'), '$15.00')
  check('formatPerMtok(null)', formatPerMtok(null), null)

  check('formatAxisDollars(0)', formatAxisDollars(0), '$0')
  check('formatAxisDollars(0.5)', formatAxisDollars(0.5), '$0.50')
  check('formatAxisDollars(2.5)', formatAxisDollars(2.5), '$2.5')
  check('formatAxisDollars(1200)', formatAxisDollars(1200), '$1.2k')

  // price form input
  check('parsePriceInput required empty', parsePriceInput('', false).ok, false)
  same('parsePriceInput optional empty', parsePriceInput(' ', true), { ok: true, value: null })
  same('parsePriceInput "$3.00"', parsePriceInput('$3.00', false), { ok: true, value: '3.00' })
  check('parsePriceInput negative', parsePriceInput('-1', false).ok, false)
  check('parsePriceInput 7dp', parsePriceInput('0.0000001', false).ok, false)
  check('parsePriceInput words', parsePriceInput('three', false).ok, false)

  // shares
  check('formatShare(null)', formatShare(null), '—')
  check('formatShare(0)', formatShare(0), '0%')
  check('formatShare(0.004) never 0%', formatShare(0.004), '<1%')
  check('formatShare(0.996) never 100%', formatShare(0.996), '>99%')
  check('formatShare(0.25)', formatShare(0.25), '25%')
  check('cachedShare no input', cachedShare({ prompt_tokens: 0, cache_read_tokens: 0 }), null)
  check('cachedShare is a split of input', cachedShare({ prompt_tokens: 1000, cache_read_tokens: 250 }), 0.25)

  // coverage caption
  check('unpricedCaption full coverage', unpricedCaption({ requests: 10, unpriced_requests: 0 }, 1), null)
  check('unpricedCaption no requests', unpricedCaption({ requests: 0, unpriced_requests: 0 }, 0), null)
  check(
    'unpricedCaption partial',
    unpricedCaption({ requests: 200, unpriced_requests: 30 }, 0.85),
    '15% of requests unpriced (30) — not included',
  )

  // grouping labels
  check(
    'SPEND_GROUPS order',
    SPEND_GROUPS.map((g) => g.label).join(','),
    'People,API keys,Models,Aliases,Routes,Backends,Providers',
  )
  check('label: class', spendRowLabel('backend_class', { key: 'external_api', label: null }), 'Outside your cloud account')
  check('label: class, own infra', spendRowLabel('backend_class', { key: 'self_hosted', label: 'x' }), 'Your infrastructure')
  check(
    'label: class unrecorded',
    spendRowLabel('backend_class', { key: null, label: null }),
    'Not recorded — refused early, or pre-upgrade',
  )
  check('label: provider', spendRowLabel('provider', { key: 'bedrock', label: null }), 'AWS Bedrock')
  check('label: user prefers server label', spendRowLabel('user', { key: 'u1', label: 'Ada Lovelace' }), 'Ada Lovelace')
  check('label: user falls back to key', spendRowLabel('user', { key: 'u1', label: '' }), 'u1')
  check('label: no route', spendRowLabel('route', { key: '', label: null }), 'Not routed')

  // chart buckets: UTC-aligned, partial ends, contiguous
  const T0 = Date.parse('2026-10-10T15:30:00Z')
  const T1 = Date.parse('2026-10-13T09:00:00Z')
  const days = timeBuckets(T0, T1, 'day')
  check('timeBuckets day count', days.length, 4)
  check('timeBuckets first starts at since', days[0]?.start, T0)
  check('timeBuckets first ends at UTC midnight', days[0]?.end, Date.parse('2026-10-11T00:00:00Z'))
  check('timeBuckets last ends at until', days[days.length - 1]?.end, T1)
  check('timeBuckets contiguous', days.every((b, i) => i === 0 || b.start === days[i - 1].end), true)
  check('timeBuckets empty range', timeBuckets(T1, T0, 'day').length, 0)
  check('timeBuckets 24h hourly', timeBuckets(T0, T0 + 24 * 3_600_000, 'hour').length, 25)

  same(
    'classSpendPoint',
    classSpendPoint([
      { key: 'self_hosted', cost_cents: '150' },
      { key: 'external_api', cost_cents: '25.5' },
      { key: 'cloud_managed', cost_cents: null },
      { key: null, cost_cents: '10' },
    ]),
    { self_hosted: 1.5, cloud_managed: 0, external_api: 0.255, other: 0.1 },
  )

  // CSV: quoting, formula guard, exact cost, unpriced empty
  check('csvCell quote', csvCell('a,"b"', true), '"a,""b"""')
  check('csvCell formula guard', csvCell('=HYPERLINK("x")', true), `"'=HYPERLINK(""x"")"`)
  check('csvCell numbers untouched', csvCell(-5), '-5')
  const csv = spendCsv('user', [
    {
      key: 'u1', label: 'Ada, L.', requests: 3, prompt_tokens: 100, completion_tokens: 20,
      cache_read_tokens: 40, cache_creation_tokens: 0, cost_cents: '1234.5678', unpriced_requests: 0,
    },
    {
      key: 'u2', label: 'Bob', requests: 1, prompt_tokens: 5, completion_tokens: 1,
      cache_read_tokens: 0, cache_creation_tokens: 0, cost_cents: null, unpriced_requests: 1,
    },
  ])
  check(
    'spendCsv',
    csv,
    'Person,key,requests,prompt_tokens,completion_tokens,cache_read_tokens,cache_creation_tokens,estimated_cost_usd_at_list_price,unpriced_requests\r\n' +
      '"Ada, L.",u1,3,100,20,40,0,12.345678,0\r\n' +
      'Bob,u2,1,5,1,0,0,,1\r\n',
  )

  // one request
  // A refused request reports no tokens; an api that omits empty fields sends no
// key at all. Found live: the Requests table crashed on "i is undefined".
check('tokenSplitText(fields absent) is null', tokenSplitText({}), null)
check('tokenSplitText(null usage) is null', tokenSplitText({ prompt_tokens: null, completion_tokens: null }), null)
check('tokenSplitText(prompt only, completion absent)', tokenSplitText({ prompt_tokens: 12 }), '12 in / — out')
check(
    'tokenSplitText with cache',
    tokenSplitText({ prompt_tokens: 1200, completion_tokens: 80, cache_read_tokens: 1000, cache_creation_tokens: 50 }),
    '1,200 in (1,000 cache read, 50 cache write) / 80 out',
  )
  check(
    'tokenSplitText no cache',
    tokenSplitText({ prompt_tokens: 12, completion_tokens: 3, cache_read_tokens: 0, cache_creation_tokens: null }),
    '12 in / 3 out',
  )
  check('tokenSplitText no usage', tokenSplitText({ prompt_tokens: null, completion_tokens: null }), null)

  check('UNPRICED_HINT', UNPRICED_HINT, 'No price for this model; add one under Prices.')
  check(
    'seedNote',
    seedNote('2026-09-30T00:00:00Z'),
    'List prices as published on September 30, 2026; vendors change them — replace a row when they do.',
  )
}

// --- the chart's one read: bucket × class -----------------------------------
//
// One group_by=day,backend_class read replaces a request per bucket. Pinned:
// empty buckets still get a point, the partial first bucket matches its
// aligned day, 'unrecorded' lands in "other", and a row outside the period or
// with an unparseable key is dropped rather than added to a neighbour.
{
  const same = (what: string, actual: unknown, expected: unknown) =>
    check(what, JSON.stringify(actual), JSON.stringify(expected))
  const T0 = Date.parse('2026-10-10T15:30:00Z')
  const T1 = Date.parse('2026-10-12T09:00:00Z')
  const buckets = timeBuckets(T0, T1, 'day')
  const zero = { self_hosted: 0, cloud_managed: 0, external_api: 0, other: 0 }
  same(
    'classTimeline',
    classTimeline(
      buckets,
      [
        { key: '2026-10-10T00:00:00Z', backend_class: 'self_hosted', cost_cents: '150' },
        { key: '2026-10-10T00:00:00Z', backend_class: 'unrecorded', cost_cents: '10' },
        { key: '2026-10-12T00:00:00Z', backend_class: 'external_api', cost_cents: '25.5' },
        { key: '2026-10-09T00:00:00Z', backend_class: 'self_hosted', cost_cents: '999' },
        { key: 'garbage', backend_class: 'self_hosted', cost_cents: '999' },
      ],
      'day',
    ),
    [{ ...zero, self_hosted: 1.5, other: 0.1 }, zero, { ...zero, external_api: 0.255 }],
  )
  const hours = timeBuckets(T0, T0 + 2 * 3_600_000, 'hour')
  same(
    'classTimeline hourly',
    classTimeline(hours, [{ key: '2026-10-10T16:00:00Z', backend_class: 'cloud_managed', cost_cents: '100' }], 'hour'),
    [zero, { ...zero, cloud_managed: 1 }, zero],
  )
}

// --- vendor billing keys and reconciliation ----------------------------------
//
// The billing key is a separate, broader credential. These pin the sentences
// that keep that visible, the state copy (above all wrong_key_type, the one
// mistake everybody makes first), the signed difference column with its
// non-colour cue, and the Pull now cooldown.
{
  check('billing key name anthropic', BILLING_KEY_NAME.anthropic, 'Admin API key')
  check('billing key name openai', BILLING_KEY_NAME.openai, 'organisation admin key')
  check(
    'billingKeyExplanation anthropic',
    billingKeyExplanation('anthropic'),
    "Reconciles Darkplum's estimates against Anthropic's own usage and cost reports. Needs an Admin API key — a different, broader credential than the inference key above: it can list your organisation's workspaces and keys. It is held by Darkplum's api only, used by one daily job, and never by the gateway.",
  )
  check(
    'billingKeyExplanation openai names the org admin key',
    billingKeyExplanation('openai').includes('Needs an organisation admin key'),
    true,
  )
  check(
    'wrong_key_type copy (anthropic)',
    billingStateCopy('anthropic', 'wrong_key_type').detail,
    'This is an inference key. The reports need an Admin API key created under Settings > Admin keys.',
  )
  check('wrong_key_type is danger', billingStateCopy('anthropic', 'wrong_key_type').variant, 'danger')
  check('pending_mount is a warning, not a danger', billingStateCopy('anthropic', 'pending_mount').variant, 'warning')
  check('pending_mount says the key is stored', billingStateCopy('anthropic', 'pending_mount').detail.includes('stored'), true)
  check('openai wrong_key_type names a project key', billingStateCopy('openai', 'wrong_key_type').detail.startsWith('This is a project key.'), true)
  check('ok badge', billingStateCopy('openai', 'ok').label, 'Connected')
  check('not_configured is dashed neutral', billingStateCopy('anthropic', 'not_configured').dashed, true)
  check('bedrock not_configured mentions Cost Explorer', billingStateCopy('bedrock', 'not_configured').detail.includes('Cost Explorer'), true)
  check('bedrock auth_failed names the IAM action', billingStateCopy('bedrock', 'auth_failed').detail.includes('ce:GetCostAndUsage'), true)
  check('unknown state named, not dropped', billingStateCopy('openai', 'mystery').label, 'Unrecognised state')
  check(
    'verify wrong_key_type reuses state copy',
    billingVerifyErrorCopy('anthropic', 'wrong_key_type', 'server words', null),
    billingStateCopy('anthropic', 'wrong_key_type').detail,
  )
  check(
    'verify vendor_error with status',
    billingVerifyErrorCopy('openai', 'vendor_error', 'x', 503),
    'OpenAI answered with an error (HTTP 503). Nothing was stored.',
  )
  check('verify unknown code uses server message', billingVerifyErrorCopy('openai', 'new_code', 'Server said so.', null), 'Server said so.')
  check('lastPulledText null', lastPulledText(null), 'Never pulled')
  check('lastPulledText garbage', lastPulledText('nope'), 'Never pulled')

  // the difference column: signed, real minus, a glyph as well as a colour
  check('formatDelta null', formatDelta(null), null)
  check('formatDelta garbage', formatDelta('x'), null)
  check('formatDelta over text', formatDelta('123')?.text, '+$1.23')
  check('formatDelta over glyph', formatDelta('123')?.glyph, '▲')
  check('formatDelta over spoken', formatDelta('123')?.spoken, '$1.23 more than the estimate')
  check('formatDelta under text (U+2212)', formatDelta('-50')?.text, '\u2212$0.50')
  check('formatDelta under direction', formatDelta('-50')?.direction, 'under')
  check('formatDelta under glyph', formatDelta('-50')?.glyph, '▼')
  check('formatDelta sub-half-cent is even', formatDelta('0.4')?.direction, 'even')
  check('formatDelta even text', formatDelta('-0.2')?.text, '$0.00')
  check('formatDelta sub-cent keeps places', formatDelta('0.5')?.text, '+$0.005')

  // Pull now
  check('PULL_COOLDOWN_MS', PULL_COOLDOWN_MS, 60_000)
  check('cooldown never pressed', pullCooldownSeconds(null, 1_000), 0)
  check('cooldown just pressed', pullCooldownSeconds(1_000, 1_000), 60)
  check('cooldown rounds up', pullCooldownSeconds(1_000, 1_500), 60)
  check('cooldown mid-way', pullCooldownSeconds(0, 30_000), 30)
  check('cooldown last ms', pullCooldownSeconds(0, 59_999), 1)
  check('cooldown over', pullCooldownSeconds(0, 60_000), 0)
  check('cooldown clock went backwards stays bounded', pullCooldownSeconds(10_000, 0), 60)
  check('cooldown custom window (429)', pullCooldownSeconds(0, 0, 120_000), 120)

  check(
    'delta note names the three causes and Bedrock',
    ['list price', 'round', 'up to a day', 'usage type'].every((w) => RECONCILIATION_DELTA_NOTE.includes(w)),
    true,
  )

  // the audit log
  check('event billing installed', EVENT_LABELS['provider.billing_key_installed']?.label, 'Vendor Billing Key Installed')
  check('event billing rotated', EVENT_LABELS['provider.billing_key_rotated']?.label, 'Vendor Billing Key Rotated')
  check('event billing removed', EVENT_LABELS['provider.billing_key_removed']?.label, 'Vendor Billing Key Removed')
  check(
    'billing events are amber',
    ['installed', 'rotated', 'removed'].every((k) => EVENT_LABELS[`provider.billing_key_${k}`]?.variant === 'warning'),
    true,
  )

  // Request parameters (GitHub #3): the audit labels, the usage-log badge,
  // and the org switch's copy.
  check('event param dropped label', eventLabel('inference.param_dropped'), 'Parameter Dropped')
  check('event param dropped is a notice', eventVariant('inference.param_dropped'), 'info')
  check('event params mode label', eventLabel('routing_policy.params_mode_changed'), 'Parameter Mode Changed')
  check('event params mode is amber', eventVariant('routing_policy.params_mode_changed'), 'warning')
  check(
    'params mode detail in words',
    formatEventDetail({ previous: 'lenient', new: 'strict' }),
    'was: drop and report, now: refuse',
  )
  check(
    'param dropped detail names the parameter first, boilerplate last',
    formatEventDetail({ note: 'n', action: 'dropped', model: 'm', param: 'seed', backend: 'anthropic', visible: 'v' }),
    'parameter: seed, action: dropped, model: m, backend: anthropic, note: n, visible: v',
  )
  check(
    'detail keys without a pin keep their stored order',
    formatEventDetail({ alias: 'fast', target_model_id: 'llama' }),
    'alias: fast, target: llama',
  )

  check('dropped badge: null', droppedParamsBadge(null), null)
  check('dropped badge: absent', droppedParamsBadge(undefined), null)
  check('dropped badge: empty', droppedParamsBadge([]), null)
  check('dropped badge: one', droppedParamsBadge(['seed']), 'Dropped · seed')
  check('dropped badge: two', droppedParamsBadge(['seed', 'logprobs']), 'Dropped · seed, logprobs')
  check(
    'dropped badge: four',
    droppedParamsBadge(['seed', 'logprobs', 'top_k', 'frequency_penalty']),
    'Dropped · seed, logprobs +2',
  )
  check(
    'dropped badge: drops before adjustments',
    droppedParamsBadge(['max_tokens=raised', 'temperature']),
    'Dropped · temperature, raised max_tokens',
  )
  check('dropped badge: adjustment only', droppedParamsBadge(['max_tokens=raised']), 'Adjusted · raised max_tokens')
  check(
    'dropped badge: unknown overflow counts N',
    droppedParamsBadge(['a', 'b', 'c', 'unknown_keys=more_7']),
    'Dropped · a, b +8',
  )
  check('dropped badge: only overflow', droppedParamsBadge(['unknown_keys=more_1']), 'Dropped · 1 unknown key')
  check(
    'dropped badge: caller-chosen name is capped',
    (droppedParamsBadge(['x'.repeat(200)]) ?? '').length,
    'Dropped · '.length + DROPPED_NAME_MAX,
  )
  check(
    'dropped title: every entry, raised worded as a change',
    droppedParamsTitle(['seed', 'logprobs', 'top_k', 'max_tokens=raised', 'unknown_keys=more_3']),
    'Not sent to the model: seed, logprobs, top_k, 3 more unknown keys. Changed by the gateway: raised max_tokens. The request was answered without them, so the output may differ from what the caller asked for.',
  )
  check(
    'dropped title: adjustment only',
    droppedParamsTitle(['max_tokens=raised']),
    'Changed by the gateway: raised max_tokens. The request was answered with that change.',
  )
  check('dropped title: none', droppedParamsTitle([]), null)
  // Only known actions are adjustments; any other "=" is part of a name.
  check('parse: max_tokens=raised is an adjustment', droppedParamsBadge(['max_tokens=raised']), 'Adjusted · raised max_tokens')
  check('parse: foo=bar is a dropped name', droppedParamsBadge(['foo=bar']), 'Dropped · foo=bar')
  check(
    'parse: foo=bar title is not reworded as an action',
    droppedParamsTitle(['foo=bar'])?.startsWith('Not sent to the model: foo=bar.'),
    true,
  )
  check('parse: unknown_keys=more_3 is an overflow of 3', droppedParamsBadge(['unknown_keys=more_3']), 'Dropped · 3 unknown keys')
  check('parse: unknown_keys=more_x is a dropped name', droppedParamsBadge(['unknown_keys=more_x']), 'Dropped · unknown_keys=more_x')
  check(
    'dropped title: nested names survive',
    droppedParamsTitle(['stream_options.include_obfuscation'])?.startsWith(
      'Not sent to the model: stream_options.include_obfuscation.',
    ),
    true,
  )

  check('params mode: null is lenient', paramsModeOf(null), 'lenient')
  check('params mode: unknown is lenient', paramsModeOf('bogus'), 'lenient')
  check('params mode: strict', paramsModeOf('strict'), 'strict')
  check('params mode options: lenient first', PARAMS_MODE_OPTIONS.map((o) => o.value).join(','), 'lenient,strict')
  check('params mode lenient label', PARAMS_MODE_OPTIONS[0].label, "Drop and report parameters a model can't use")
  check('params mode strict label', PARAMS_MODE_OPTIONS[1].label, 'Refuse requests that include them')
  check(
    'lenient consequence says dropping changes outputs',
    ['seed', 'penalty', 'changes the output'].every((w) => PARAMS_MODE_OPTIONS[0].consequence.includes(w)),
    true,
  )
  check(
    'strict consequence says 400 for every key',
    ['400', 'Every key'].every((w) => PARAMS_MODE_OPTIONS[1].consequence.includes(w)),
    true,
  )
  check(
    'params note: always-refused and rollout',
    ['always refused', 'gateways running this release'].every((w) => PARAMS_MODE_NOTE.includes(w)),
    true,
  )
  check('strict confirm names the blast radius', PARAMS_MODE_STRICT_CONFIRM.description.includes('every API key'), true)
  check('params chip lenient', paramsModeChip('lenient'), 'Parameters: dropped and reported')
  check('params chip strict', paramsModeChip('strict'), 'Parameters: refused')
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
  `Residency truth table holds (${table.cells.length} cells from ${RESIDENCY_TRUTH_TABLE_PATH} + ladder order + restricted rule + class rank, model fallback, empty-state claim, vendor rate-limit wait, who a vendor failure names, and the vendor-key helpers: glob matching, filter serialisation over every subset of a catalogue, the checklist's headings, what a Replace opens on, the key entry line, the installing and removing windows, the transport gate, which of the two transport faults each refusal names, the admitted diff, the Bedrock data-retention opt-in copy, and the routed-alias on_route_unavailable rule: reason text, the setting x class outcome matrix, route hints, the classification-failure warning, the route_target_unavailable refusal, and what a disable does to each alias; and the spend helpers: cost display, exact CSV dollars, $/MTok, price input, shares, coverage caption, grouping labels, chart buckets, the one-read class timeline; and the billing-key helpers: state copy, verify errors, the signed difference, the Pull now cooldown, the audit labels; and the request-parameter helpers: the dropped badge and its title, the params-mode copy and audit labels).`,
)
