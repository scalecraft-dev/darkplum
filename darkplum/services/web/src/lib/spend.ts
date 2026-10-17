import type { Decimal, SpendGroupBy, SpendRow, SpendTotals } from '../types'
import { providerLabel } from './modelRouting.ts'

/**
 * Estimated spend, as the Activity > Spend view and the Prices editor show it.
 *
 * Everything here is an estimate at published list price, computed from
 * Darkplum's own request records. That is said once, on the page, in
 * SPEND_BASIS_NOTE — not repeated beside every number, where it would become
 * wallpaper nobody reads.
 *
 * Money arrives as decimal strings (US cents) so nothing is lost on the wire.
 * Display rounds; the CSV does not.
 */

/** The one sentence that frames every amount on the Spend view. */
export const SPEND_BASIS_NOTE =
  "Every amount here is an estimate at published list price, computed from Darkplum's own request records. It is not your invoice: discounts, commitments and credits are not applied."

/** Said wherever an amount is missing because no price row matched. */
export const UNPRICED_HINT = 'No price for this model; add one under Prices.'

/** The line above the Prices table. */
export function seedNote(seedGeneratedAt: string | null | undefined): string {
  const date = formatDateOnly(seedGeneratedAt)
  return date
    ? `List prices as published on ${date}; vendors change them — replace a row when they do.`
    : 'List prices as published by each vendor; vendors change them — replace a row when they do.'
}

function formatDateOnly(iso: string | null | undefined): string | null {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  return d.toLocaleDateString('en-US', { year: 'numeric', month: 'long', day: 'numeric', timeZone: 'UTC' })
}

// ---------------------------------------------------------------------------
// Decimal strings
// ---------------------------------------------------------------------------

const DECIMAL_RE = /^-?\d+(\.\d+)?$/

/** A decimal string (or number) as a number, or null when it is not one. */
export function decimalToNumber(v: Decimal | null | undefined): number | null {
  if (v === null || v === undefined) return null
  if (typeof v === 'number') return Number.isFinite(v) ? v : null
  const t = v.trim()
  if (!DECIMAL_RE.test(t)) return null
  return Number(t)
}

/**
 * Cents → dollars as an exact decimal string, by moving the point two places
 * rather than dividing a float: "1234.5678" → "12.345678", "5" → "0.05".
 * null when the input is not a decimal. Used by the CSV, which must not round.
 */
export function centsToDollarString(cents: Decimal | null | undefined): string | null {
  if (cents === null || cents === undefined) return null
  const raw = typeof cents === 'number' ? String(cents) : cents.trim()
  if (!DECIMAL_RE.test(raw)) return null
  const negative = raw.startsWith('-')
  const body = negative ? raw.slice(1) : raw
  const [intPart, fracPart = ''] = body.split('.')
  const padded = intPart.padStart(3, '0')
  const whole = padded.slice(0, -2).replace(/^0+(?=\d)/, '')
  const frac = (padded.slice(-2) + fracPart).replace(/0+$/, '')
  const out = frac ? `${whole}.${frac}` : whole
  return negative && out !== '0' ? `-${out}` : out
}

const USD = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' })

/**
 * Cents → what a person reads. null means "unpriced" and the caller renders
 * its own dash with UNPRICED_HINT; this never invents a $0.00 for a missing
 * price.
 *
 * At a cent and up: ordinary currency, two places. Below a cent — one short
 * request on a cheap model — up to four places, so a real cost does not round
 * to $0.00 and look free; below that, "<$0.0001".
 */
export function formatCost(cents: Decimal | null | undefined): string | null {
  const n = decimalToNumber(cents ?? null)
  if (n === null) return null
  const dollars = n / 100
  if (dollars === 0) return '$0.00'
  const abs = Math.abs(dollars)
  if (abs >= 0.01) return USD.format(dollars)
  if (abs < 0.0001) return dollars < 0 ? '-<$0.0001' : '<$0.0001'
  return `${dollars < 0 ? '-' : ''}$${trimZeros(abs.toFixed(4))}`
}

/** Compact dollars for a chart axis: $0, $0.50, $12, $1.2k. Input is dollars. */
export function formatAxisDollars(dollars: number): string {
  if (dollars === 0) return '$0'
  const abs = Math.abs(dollars)
  if (abs >= 1000) return `$${trimZeros((dollars / 1000).toFixed(1))}k`
  if (abs >= 10) return `$${Math.round(dollars)}`
  if (abs >= 1) return `$${trimZeros(dollars.toFixed(1))}`
  return `$${dollars.toFixed(2)}`
}

/** A per-million-token price: at least two places, at most four. "3" → "$3.00",
 *  "0.075" → "$0.075", "1.23456" → "$1.2346". null for a missing price. */
export function formatPerMtok(v: Decimal | null | undefined): string | null {
  const n = decimalToNumber(v ?? null)
  if (n === null) return null
  let s = n.toFixed(4)
  // Trim trailing zeros past the second decimal place.
  s = s.replace(/(\.\d\d\d*?)0+$/, '$1')
  return `$${s}`
}

function trimZeros(s: string): string {
  return s.includes('.') ? s.replace(/\.?0+$/, '') : s
}

/**
 * What the price form accepts for one $/MTok field: a non-negative decimal
 * with at most six places, a leading "$" tolerated. Empty is allowed only for
 * an optional field (the cache prices) and yields null.
 */
export function parsePriceInput(
  raw: string,
  optional: boolean,
): { ok: true; value: string | null } | { ok: false; error: string } {
  const t = raw.trim().replace(/^\$/, '').trim()
  if (t === '') {
    return optional ? { ok: true, value: null } : { ok: false, error: 'Required' }
  }
  if (!/^\d+(\.\d+)?$/.test(t)) return { ok: false, error: 'A price in dollars, e.g. 3 or 0.075' }
  const places = t.split('.')[1]?.length ?? 0
  if (places > 6) return { ok: false, error: 'At most six decimal places' }
  return { ok: true, value: t }
}

// ---------------------------------------------------------------------------
// Shares
// ---------------------------------------------------------------------------

/** A 0..1 share as a percentage that never says 0% for something non-zero
 *  or 100% for something short of it. null in → '—'. */
export function formatShare(share: number | null): string {
  if (share === null || !Number.isFinite(share)) return '—'
  if (share <= 0) return '0%'
  if (share >= 1) return '100%'
  const pct = share * 100
  if (pct < 1) return '<1%'
  if (pct > 99) return '>99%'
  return `${Math.round(pct)}%`
}

/** Cache reads as a share of input tokens. Cache tokens are a split of
 *  prompt_tokens, never in addition to it. null with no input tokens. */
export function cachedShare(t: Pick<SpendTotals, 'prompt_tokens' | 'cache_read_tokens'>): number | null {
  if (!t.prompt_tokens) return null
  return Math.min(1, Math.max(0, t.cache_read_tokens / t.prompt_tokens))
}

/**
 * The caption under the spend tile when some requests went unpriced, or null
 * when every request was priced (or there were none). Leads with the share,
 * because "is this number roughly the whole picture?" is the question.
 */
export function unpricedCaption(
  totals: Pick<SpendTotals, 'requests' | 'unpriced_requests'>,
  pricedCoverage: number,
): string | null {
  if (totals.requests === 0) return null
  if (pricedCoverage >= 1 && totals.unpriced_requests === 0) return null
  const share = pricedCoverage >= 1 ? totals.unpriced_requests / totals.requests : 1 - pricedCoverage
  const n = totals.unpriced_requests
  return `${formatShare(share)} of requests unpriced (${n.toLocaleString('en-US')}) — not included`
}

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

/** The table's group-by choices, in the order the selector lists them. `day`
 *  is a grouping the API offers but is the chart's job, not the table's. */
export const SPEND_GROUPS: ReadonlyArray<{ value: Exclude<SpendGroupBy, 'day'>; label: string; column: string }> = [
  { value: 'user',          label: 'People',    column: 'Person' },
  { value: 'api_key',       label: 'API keys',  column: 'API key' },
  { value: 'model',         label: 'Models',    column: 'Model' },
  { value: 'alias',         label: 'Aliases',   column: 'Alias' },
  { value: 'route',         label: 'Routes',    column: 'Route' },
  { value: 'backend_class', label: 'Backends',  column: 'Where it ran' },
  { value: 'provider',      label: 'Providers', column: 'Provider' },
]

/** The three places a request can run, closest first — the stacking order of
 *  the chart and the reading order everywhere else. Same words as the Where it
 *  ran filter on the Requests view. */
export const SPEND_CLASSES = ['self_hosted', 'cloud_managed', 'external_api'] as const
export type SpendClass = (typeof SPEND_CLASSES)[number]

export const SPEND_CLASS_LABELS: Record<SpendClass, string> = {
  self_hosted:   'Your infrastructure',
  cloud_managed: 'Your cloud account',
  external_api:  'Outside your cloud account',
}

/** What a row with no value for the grouping is called. Each says why it is
 *  empty, rather than one "(none)" that reads like missing data everywhere. */
const EMPTY_LABELS: Record<SpendGroupBy, string> = {
  user:          'No person (API key only)',
  api_key:       'No API key (signed-in chat)',
  model:         'No model resolved',
  provider:      'No provider recorded',
  backend_class: 'Not recorded — refused early, or pre-upgrade',
  alias:         'No alias (model asked for directly)',
  route:         'Not routed',
  day:           'No date',
}

/** The words a spend row is listed under for a grouping. */
export function spendRowLabel(group: SpendGroupBy, row: Pick<SpendRow, 'key' | 'label'>): string {
  const key = row.key ?? ''
  if (group === 'backend_class') {
    if (key in SPEND_CLASS_LABELS) return SPEND_CLASS_LABELS[key as SpendClass]
    return key ? key : EMPTY_LABELS.backend_class
  }
  if (group === 'provider') return key ? providerLabel(key) : EMPTY_LABELS.provider
  const label = row.label?.trim()
  if (label) return label
  return key ? key : EMPTY_LABELS[group]
}

/** Whether a grouping's key is a technical identifier worth showing in mono
 *  with a copy button (model ids, aliases, route names). */
export function isIdentifierGroup(group: SpendGroupBy): boolean {
  return group === 'model' || group === 'alias' || group === 'route'
}

// ---------------------------------------------------------------------------
// Chart buckets
// ---------------------------------------------------------------------------

/**
 * Time buckets for the spend chart, aligned to UTC hour or day boundaries so
 * they line up with how the API groups by day. The first and last buckets are
 * partial (they start at `sinceMs` and end at `untilMs`). Pure: the caller
 * supplies the clock.
 */
export function timeBuckets(
  sinceMs: number,
  untilMs: number,
  unit: 'hour' | 'day',
): Array<{ start: number; end: number }> {
  if (!(untilMs > sinceMs)) return []
  const step = unit === 'hour' ? 3_600_000 : 86_400_000
  const out: Array<{ start: number; end: number }> = []
  let start = sinceMs
  let boundary = Math.floor(sinceMs / step) * step + step
  while (start < untilMs) {
    const end = Math.min(boundary, untilMs)
    out.push({ start, end })
    start = end
    boundary += step
  }
  return out
}

/** One chart point from a bucket's group_by=backend_class rows: priced cost
 *  per class, in dollars, plus anything recorded under no known class. */
export function classSpendPoint(
  rows: ReadonlyArray<Pick<SpendRow, 'key' | 'cost_cents'>>,
): Record<SpendClass | 'other', number> {
  const point: Record<SpendClass | 'other', number> = {
    self_hosted: 0,
    cloud_managed: 0,
    external_api: 0,
    other: 0,
  }
  for (const r of rows) {
    const cents = decimalToNumber(r.cost_cents)
    if (cents === null) continue
    const k = (SPEND_CLASSES as readonly string[]).includes(r.key ?? '') ? (r.key as SpendClass) : 'other'
    point[k] += cents / 100
  }
  return point
}

/**
 * The chart's points from ONE group_by=day|hour,backend_class read.
 *
 * Every bucket in `buckets` gets a point, zero when the api sent no row for it
 * (it omits empty buckets), so the x-axis is the whole period and a quiet day
 * reads as a gap rather than vanishing. Rows are matched on their UTC bucket
 * start (`key`, RFC3339); the first and last buckets are partial, so a bucket
 * is matched by the aligned start it falls in, not by its own start. A row
 * whose key does not parse, or falls outside the period, is dropped rather
 * than added to a neighbour.
 */
export function classTimeline(
  buckets: ReadonlyArray<{ start: number; end: number }>,
  rows: ReadonlyArray<Pick<SpendRow, 'key' | 'cost_cents' | 'backend_class'>>,
  unit: 'hour' | 'day',
): Array<Record<SpendClass | 'other', number>> {
  const step = unit === 'hour' ? 3_600_000 : 86_400_000
  const index = new Map<number, number>()
  buckets.forEach((b, i) => index.set(Math.floor(b.start / step) * step, i))
  const byBucket = buckets.map(() => [] as Array<Pick<SpendRow, 'key' | 'cost_cents'>>)
  for (const r of rows) {
    const t = Date.parse(r.key ?? '')
    if (Number.isNaN(t)) continue
    const i = index.get(Math.floor(t / step) * step)
    if (i === undefined) continue
    byBucket[i].push({ key: r.backend_class ?? null, cost_cents: r.cost_cents })
  }
  return byBucket.map((list) => classSpendPoint(list))
}

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

/** One CSV cell. Quoted when it must be; a text cell that a spreadsheet would
 *  run as a formula (leading = + - @, tab or CR) is prefixed with an
 *  apostrophe, because a user's display name is not a trusted string. */
export function csvCell(value: string | number | null | undefined, text = false): string {
  if (value === null || value === undefined) return ''
  let s = String(value)
  if (text && /^[=+\-@\t\r]/.test(s)) s = `'${s}`
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s
}

/**
 * The current grouping as CSV, from the rows on screen. The cost column's
 * name carries the basis, once; its values are exact dollars (no rounding),
 * empty when unpriced.
 */
export function spendCsv(group: Exclude<SpendGroupBy, 'day'>, rows: readonly SpendRow[]): string {
  const column = SPEND_GROUPS.find((g) => g.value === group)?.column ?? group
  const header = [
    column,
    'key',
    'requests',
    'prompt_tokens',
    'completion_tokens',
    'cache_read_tokens',
    'cache_creation_tokens',
    'estimated_cost_usd_at_list_price',
    'unpriced_requests',
  ].map((h) => csvCell(h, true))
  const lines = [header.join(',')]
  for (const r of rows) {
    lines.push(
      [
        csvCell(spendRowLabel(group, r), true),
        csvCell(r.key ?? '', true),
        csvCell(r.requests),
        csvCell(r.prompt_tokens),
        csvCell(r.completion_tokens),
        csvCell(r.cache_read_tokens),
        csvCell(r.cache_creation_tokens),
        csvCell(centsToDollarString(r.cost_cents)),
        csvCell(r.unpriced_requests),
      ].join(','),
    )
  }
  return lines.join('\r\n') + '\r\n'
}

// ---------------------------------------------------------------------------
// One request
// ---------------------------------------------------------------------------

/**
 * Tokens on one recorded request, with the cache split when there is one:
 * "1,200 in (1,000 cache read, 50 cache write) / 80 out". null when the
 * backend reported no usage.
 */
export function tokenSplitText(e: {
  prompt_tokens?: number | null
  completion_tokens?: number | null
  cache_read_tokens?: number | null
  cache_creation_tokens?: number | null
}): string | null {
  // A request that reported no usage (a refusal, an upstream failure) carries
  // null tokens — and from an api that omits empty fields, no key at all. Both
  // mean "unknown", never 0.
  const prompt = e.prompt_tokens ?? null
  const completion = e.completion_tokens ?? null
  if (prompt === null && completion === null) return null
  const fmt = (n: number | null | undefined) => (n == null ? '—' : n.toLocaleString('en-US'))
  const parts: string[] = []
  if (e.cache_read_tokens) parts.push(`${fmt(e.cache_read_tokens)} cache read`)
  if (e.cache_creation_tokens) parts.push(`${fmt(e.cache_creation_tokens)} cache write`)
  const split = parts.length ? ` (${parts.join(', ')})` : ''
  return `${fmt(prompt)} in${split} / ${fmt(completion)} out`
}
