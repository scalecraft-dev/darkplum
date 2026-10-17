import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { useTheme } from '../context/ThemeContext'
import { getSpend, getSpendTimeline } from '../lib/api'
import {
  SPEND_BASIS_NOTE,
  SPEND_CLASSES,
  SPEND_CLASS_LABELS,
  SPEND_GROUPS,
  UNPRICED_HINT,
  cachedShare,
  classTimeline,
  decimalToNumber,
  formatAxisDollars,
  formatCost,
  formatShare,
  isIdentifierGroup,
  spendCsv,
  spendRowLabel,
  timeBuckets,
  unpricedCaption,
  type SpendClass,
} from '../lib/spend'
import { saveBlob } from '../lib/download'
import type { SpendGroupBy, SpendReport, SpendRow, TimeRange } from '../types'
import { useToast } from '../components/Toast'
import { Button, CopyButton, EgressGlyph, Skeleton, Table, TableEmpty } from '../components/ui'
import activityStyles from './ActivityTab.module.css'
import styles from './SpendView.module.css'
import { ReconciliationSection } from './ReconciliationSection'

type TableGroup = Exclude<SpendGroupBy, 'day'>
type SortKey = 'label' | 'requests' | 'prompt_tokens' | 'completion_tokens' | 'cached' | 'cost' | 'unpriced'

const ROW_LIMIT = 200
/**
 * Class colours for the chart, per theme. Hex because CSS variables are
 * unreliable in SVG attributes (the same reason as ActivityTab's chart).
 *
 * They follow the class badge vocabulary — neutral/moss for your own
 * infrastructure, grape/lilac (info) for your cloud account, amber (warning)
 * for outside it — and are separated by lightness as well as hue, so the
 * three stay distinct under deuteranopia and in greyscale. Every fill clears
 * 3:1 against its theme's card surface (WCAG 1.4.11). Outside-your-
 * account is also hatched: the one class that crosses the boundary carries a
 * second, non-colour mark, as it does everywhere else (the egress glyph).
 */
const CLASS_COLORS = {
  dark: {
    self_hosted: '#5C7048',
    cloud_managed: '#A877D9',
    external_api: '#D4AB55',
    external_hatch: '#7d5a0e',
    other: '#6e6460',
    grid: '#1A1E2A',
    axis: '#929bab',
    separator: '#131721',
  },
  light: {
    self_hosted: '#5C7048',
    cloud_managed: '#6B2FA8',
    external_api: '#9a7420',
    external_hatch: '#d9b768',
    other: '#7a7068',
    grid: '#d4cec4',
    axis: '#5f5446',
    separator: '#e8e2d6',
  },
} as const

function rangeBounds(range: TimeRange): { since: number; until: number; unit: 'hour' | 'day' } {
  const until = Date.now()
  const hours = range === '24h' ? 24 : range === '7d' ? 24 * 7 : 24 * 30
  return { since: until - hours * 3_600_000, until, unit: range === '24h' ? 'hour' : 'day' }
}

function bucketLabel(start: number, unit: 'hour' | 'day'): string {
  const d = new Date(start)
  return unit === 'hour'
    ? d.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: 'UTC' })
    : d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
}

type ChartPoint = { label: string } & Record<SpendClass | 'other', number>

function sortValue(row: SpendRow, key: SortKey, group: TableGroup): string | number {
  switch (key) {
    case 'label':
      return spendRowLabel(group, row).toLowerCase()
    case 'cached':
      return cachedShare(row) ?? -1
    case 'cost':
      return decimalToNumber(row.cost_cents) ?? -1
    case 'unpriced':
      return row.unpriced_requests
    default:
      return row[key]
  }
}

// ---------------------------------------------------------------------------
// Chart tooltip
// ---------------------------------------------------------------------------

function SpendTooltip({
  active,
  payload,
  label,
  showOther,
}: {
  active?: boolean
  payload?: Array<{ payload: ChartPoint }>
  label?: string
  showOther: boolean
}) {
  if (!active || !payload?.length) return null
  const p = payload[0].payload
  const total = SPEND_CLASSES.reduce((s, c) => s + p[c], 0) + p.other
  return (
    <div className={activityStyles.tooltip}>
      <div className={activityStyles.tooltipLabel}>{label} (UTC)</div>
      <dl className={styles.tooltipList}>
        {[...SPEND_CLASSES].reverse().map((c) => (
          <div key={c} className={styles.tooltipRow}>
            <dt>
              {c === 'external_api' && <EgressGlyph />}
              {SPEND_CLASS_LABELS[c]}
            </dt>
            <dd>{formatCost(p[c] * 100)}</dd>
          </div>
        ))}
        {showOther && (
          <div className={styles.tooltipRow}>
            <dt>Not recorded</dt>
            <dd>{formatCost(p.other * 100)}</dd>
          </div>
        )}
        <div className={`${styles.tooltipRow} ${styles.tooltipTotal}`}>
          <dt>Total</dt>
          <dd>{formatCost(total * 100)}</dd>
        </div>
      </dl>
    </div>
  )
}

function Swatch({ cls, colors }: { cls: SpendClass | 'other'; colors: (typeof CLASS_COLORS)['dark' | 'light'] }) {
  const fill = colors[cls]
  return (
    <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true" className={styles.swatch}>
      <rect width="10" height="10" rx="2" fill={fill} />
      {cls === 'external_api' && (
        <path d="M-1 3 L3 -1 M-1 8 L8 -1 M4 11 L11 4" stroke={colors.external_hatch} strokeWidth="1.5" />
      )}
    </svg>
  )
}

// ---------------------------------------------------------------------------
// SpendView
// ---------------------------------------------------------------------------

export function SpendView({ timeRange, active }: { timeRange: TimeRange; active: boolean }) {
  const { toast } = useToast()
  const { theme } = useTheme()
  const colors = CLASS_COLORS[theme]

  const [group, setGroup] = useState<TableGroup>('user')
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: 'cost', desc: true })

  const [summary, setSummary] = useState<SpendReport | null>(null)
  const [table, setTable] = useState<SpendReport | null>(null)
  const [chart, setChart] = useState<ChartPoint[]>([])
  const [loadingSummary, setLoadingSummary] = useState(true)
  const [loadingTable, setLoadingTable] = useState(true)
  const [loadingChart, setLoadingChart] = useState(true)
  const [error, setError] = useState<string | null>(null)
  /** What has already been fetched, so switching back to this view (or a
   *  hidden panel re-rendering) does not re-read the same thing. */
  const [loadedRange, setLoadedRange] = useState<TimeRange | null>(null)
  const [loadedGroup, setLoadedGroup] = useState<string | null>(null)

  const unit = timeRange === '24h' ? 'hour' : 'day'

  const fetchSummaryAndChart = useCallback(async () => {
    const { since, until, unit: u } = rangeBounds(timeRange)
    setLoadingSummary(true)
    setLoadingChart(true)
    setError(null)
    // One read: bucket × class rows for the chart, and the whole period's
    // totals for the tiles. `limit` counts buckets, so it is the number of
    // buckets drawn — the api's default (24 hours) would drop the 25th,
    // partial, hour of a 24h window.
    const buckets = timeBuckets(since, until, u)
    try {
      const report = await getSpendTimeline({
        group_by: u === 'hour' ? 'hour,backend_class' : 'day,backend_class',
        since: new Date(since).toISOString(),
        until: new Date(until).toISOString(),
        limit: Math.max(1, buckets.length),
      })
      setSummary(report)
      const points = classTimeline(buckets, report.rows, u)
      setChart(buckets.map((b, i) => ({ label: bucketLabel(b.start, u), ...points[i] })))
    } catch (err) {
      setSummary(null)
      setChart([])
      setError((err as Error).message)
      toast((err as Error).message, 'error')
    } finally {
      setLoadingSummary(false)
      setLoadingChart(false)
    }
  }, [timeRange, toast])

  const fetchTable = useCallback(async () => {
    const { since, until } = rangeBounds(timeRange)
    setLoadingTable(true)
    try {
      setTable(
        await getSpend({
          group_by: group,
          since: new Date(since).toISOString(),
          until: new Date(until).toISOString(),
          limit: ROW_LIMIT,
        }),
      )
    } catch (err) {
      setTable(null)
      toast((err as Error).message, 'error')
    } finally {
      setLoadingTable(false)
    }
  }, [timeRange, group, toast])

  useEffect(() => {
    if (!active || loadedRange === timeRange) return
    setLoadedRange(timeRange)
    void fetchSummaryAndChart()
  }, [active, timeRange, loadedRange, fetchSummaryAndChart])

  useEffect(() => {
    const k = `${timeRange}:${group}`
    if (!active || loadedGroup === k) return
    setLoadedGroup(k)
    void fetchTable()
  }, [active, timeRange, group, loadedGroup, fetchTable])

  const rows = useMemo(() => {
    const list = [...(table?.rows ?? [])]
    list.sort((a, b) => {
      const av = sortValue(a, sort.key, group)
      const bv = sortValue(b, sort.key, group)
      const cmp = typeof av === 'string' ? av.localeCompare(String(bv)) : av - (bv as number)
      return sort.desc ? -cmp : cmp
    })
    return list
  }, [table, sort, group])

  const totals = summary?.totals
  const costText = totals ? formatCost(totals.cost_cents) : null
  const caption = totals && summary ? unpricedCaption(totals, summary.priced_coverage) : null
  const cached = totals ? cachedShare(totals) : null
  const showOther = chart.some((p) => p.other > 0)
  const chartEmpty = chart.every((p) => SPEND_CLASSES.every((c) => p[c] === 0) && p.other === 0)
  const groupMeta = SPEND_GROUPS.find((g) => g.value === group) ?? SPEND_GROUPS[0]

  function handleExport() {
    const csv = spendCsv(group, rows)
    const day = new Date().toISOString().slice(0, 10)
    saveBlob(new Blob([csv], { type: 'text/csv;charset=utf-8' }), `estimated_spend_by_${group}_${timeRange}_${day}.csv`)
  }

  function headerCell(key: SortKey, label: string, numeric = true) {
    const activeSort = sort.key === key
    return (
      <th
        aria-sort={activeSort ? (sort.desc ? 'descending' : 'ascending') : 'none'}
        className={numeric ? styles.num : undefined}
      >
        <button
          type="button"
          className={styles.sortBtn}
          onClick={() =>
            setSort((s) => (s.key === key ? { key, desc: !s.desc } : { key, desc: key !== 'label' }))
          }
        >
          {label}
          <span className={styles.sortMark} aria-hidden="true">
            {activeSort ? (sort.desc ? '↓' : '↑') : ''}
          </span>
        </button>
      </th>
    )
  }

  return (
    <div>
      {/* Said once, here, for every number below. */}
      <p className={styles.basisNote}>{SPEND_BASIS_NOTE}</p>

      <div className={activityStyles.statCards}>
        <div className={activityStyles.statCard}>
          <div className={activityStyles.statLabel}>Estimated spend</div>
          {loadingSummary ? (
            <Skeleton width="60%" height="28px" />
          ) : (
            <>
              <div className={activityStyles.statValue}>{costText ?? '—'}</div>
              <div className={activityStyles.statSub}>
                {costText === null && totals && totals.requests > 0 ? (
                  <>
                    No request in this period matched a price.{' '}
                    <Link to="/admin/models?tab=prices" className={styles.inlineLink}>Add one under Prices</Link>
                  </>
                ) : (
                  'USD, at list price'
                )}
              </div>
              {caption && (
                <div className={`${activityStyles.statSub} ${styles.unpricedSub}`}>
                  {caption}
                </div>
              )}
            </>
          )}
        </div>

        <div className={activityStyles.statCard}>
          <div className={activityStyles.statLabel}>Tokens in</div>
          {loadingSummary ? (
            <Skeleton width="50%" height="28px" />
          ) : (
            <>
              <div className={activityStyles.statValue}>{(totals?.prompt_tokens ?? 0).toLocaleString()}</div>
              <div className={activityStyles.statSub}>
                over {(totals?.requests ?? 0).toLocaleString()} requests
              </div>
            </>
          )}
        </div>

        <div className={activityStyles.statCard}>
          <div className={activityStyles.statLabel}>Tokens out</div>
          {loadingSummary ? (
            <Skeleton width="50%" height="28px" />
          ) : (
            <>
              <div className={activityStyles.statValue}>{(totals?.completion_tokens ?? 0).toLocaleString()}</div>
              <div className={activityStyles.statSub}>completion tokens</div>
            </>
          )}
        </div>

        <div className={activityStyles.statCard}>
          <div className={activityStyles.statLabel}>Cached input</div>
          {loadingSummary ? (
            <Skeleton width="40%" height="28px" />
          ) : (
            <>
              <div className={activityStyles.statValue}>{formatShare(cached)}</div>
              <div className={activityStyles.statSub}>
                {(totals?.cache_read_tokens ?? 0).toLocaleString()} read from cache ·{' '}
                {(totals?.cache_creation_tokens ?? 0).toLocaleString()} written
              </div>
            </>
          )}
        </div>
      </div>

      {/* Spend over time, stacked by where it ran */}
      <div className={activityStyles.chartCard}>
        <div className={activityStyles.chartHeader}>
          <span className={activityStyles.chartTitle} id="spend-chart-title">
            Estimated spend per {unit} (UTC), by where it ran
          </span>
          <ul className={styles.legend} aria-label="Legend">
            {SPEND_CLASSES.map((c) => (
              <li key={c} className={styles.legendItem}>
                <Swatch cls={c} colors={colors} />
                {c === 'external_api' && <EgressGlyph />}
                {SPEND_CLASS_LABELS[c]}
              </li>
            ))}
            {showOther && (
              <li className={styles.legendItem}>
                <Swatch cls="other" colors={colors} />
                Not recorded
              </li>
            )}
          </ul>
        </div>
        {loadingChart ? (
          <Skeleton width="100%" height="240px" borderRadius="var(--radius-md)" />
        ) : error ? (
          <div className={styles.chartEmpty} role="status">
            Spend could not be loaded: {error}
            <Button variant="secondary" size="sm" onClick={() => { setLoadedRange(null) }}>Try again</Button>
          </div>
        ) : chartEmpty ? (
          <div className={styles.chartEmpty}>
            No priced requests in this period.
            {totals && totals.unpriced_requests > 0 && (
              <Link to="/admin/models?tab=prices" className={styles.inlineLink}>Add a price under Prices</Link>
            )}
          </div>
        ) : (
          // The per-class numbers are in the table below (group by Backends)
          // and in the tooltip; the chart is labelled so a screen reader knows
          // what it is skipping.
          <div role="img" aria-labelledby="spend-chart-title">
            <ResponsiveContainer width="100%" height={240}>
              <BarChart data={chart} margin={{ top: 4, right: 4, left: -8, bottom: 0 }} barCategoryGap="20%">
                <defs>
                  <pattern id="spendEgressHatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
                    <rect width="6" height="6" fill={colors.external_api} />
                    <line x1="0" y1="0" x2="0" y2="6" stroke={colors.external_hatch} strokeWidth="1.5" />
                  </pattern>
                </defs>
                <CartesianGrid strokeDasharray="3 3" stroke={colors.grid} vertical={false} />
                <XAxis
                  dataKey="label"
                  tick={{ fill: colors.axis, fontSize: 11 }}
                  axisLine={false}
                  tickLine={false}
                  interval="preserveStartEnd"
                />
                <YAxis
                  tick={{ fill: colors.axis, fontSize: 11 }}
                  axisLine={false}
                  tickLine={false}
                  tickFormatter={(v: number) => formatAxisDollars(v)}
                />
                <Tooltip
                  content={<SpendTooltip showOther={showOther} />}
                  cursor={{ fill: colors.grid, opacity: 0.4 }}
                />
                <Bar dataKey="self_hosted" stackId="spend" fill={colors.self_hosted} stroke={colors.separator} strokeWidth={1} isAnimationActive={false} />
                <Bar dataKey="cloud_managed" stackId="spend" fill={colors.cloud_managed} stroke={colors.separator} strokeWidth={1} isAnimationActive={false} />
                <Bar dataKey="external_api" stackId="spend" fill="url(#spendEgressHatch)" stroke={colors.separator} strokeWidth={1} isAnimationActive={false} />
                {showOther && (
                  <Bar dataKey="other" stackId="spend" fill={colors.other} stroke={colors.separator} strokeWidth={1} isAnimationActive={false} />
                )}
              </BarChart>
            </ResponsiveContainer>
          </div>
        )}
      </div>

      {/* Attribution table */}
      <div className={styles.tableHeader}>
        <div className={activityStyles.logToggle} role="group" aria-label="Group spend by">
          {SPEND_GROUPS.map((g) => (
            <button
              key={g.value}
              type="button"
              className={`${activityStyles.logToggleBtn} ${group === g.value ? activityStyles.logToggleActive : ''}`}
              onClick={() => setGroup(g.value)}
              aria-pressed={group === g.value}
            >
              {g.label}
            </button>
          ))}
        </div>
        <Button variant="secondary" size="sm" onClick={handleExport} disabled={loadingTable || rows.length === 0}>
          Export {groupMeta.label.toLowerCase()} CSV
        </Button>
      </div>

      <div className={activityStyles.tableCard}>
        <Table label={`Estimated spend by ${groupMeta.column.toLowerCase()}`}>
          <thead>
            <tr>
              {headerCell('label', groupMeta.column, false)}
              {headerCell('requests', 'Requests')}
              {headerCell('prompt_tokens', 'Tokens in')}
              {headerCell('completion_tokens', 'Tokens out')}
              {headerCell('cached', 'Cached')}
              {headerCell('cost', 'Est. cost')}
              {headerCell('unpriced', 'Unpriced')}
            </tr>
          </thead>
          <tbody>
            {loadingTable ? (
              Array.from({ length: 5 }).map((_, i) => (
                <tr key={i}>
                  {Array.from({ length: 7 }).map((__, j) => (
                    <td key={j}><Skeleton width={`${50 + ((i + j) * 13) % 40}%`} height="14px" /></td>
                  ))}
                </tr>
              ))
            ) : rows.length === 0 ? (
              <TableEmpty colSpan={7}>
                <div className={activityStyles.empty}>
                  <span className={activityStyles.emptyTitle}>No requests in this period</span>
                  <span className={activityStyles.emptyHint}>Widen the time range above.</span>
                </div>
              </TableEmpty>
            ) : (
              rows.map((r, i) => {
                const label = spendRowLabel(group, r)
                const cost = formatCost(r.cost_cents)
                const isId = isIdentifierGroup(group) && !!r.key
                return (
                  <tr key={`${r.key ?? ''}-${i}`}>
                    <td>
                      <div className={styles.labelCell}>
                        {group === 'backend_class' && r.key === 'external_api' && <EgressGlyph />}
                        {isId ? <code className={styles.mono}>{label}</code> : <span>{label}</span>}
                        {isId && r.key && <CopyButton value={r.key} label={`Copy ${groupMeta.column.toLowerCase()}`} />}
                      </div>
                    </td>
                    <td className={styles.num}>{r.requests.toLocaleString()}</td>
                    <td className={styles.num}>{r.prompt_tokens.toLocaleString()}</td>
                    <td className={styles.num}>{r.completion_tokens.toLocaleString()}</td>
                    <td
                      className={styles.num}
                      title={`${r.cache_read_tokens.toLocaleString()} read from cache · ${r.cache_creation_tokens.toLocaleString()} written to cache`}
                    >
                      {formatShare(cachedShare(r))}
                    </td>
                    <td className={styles.num}>
                      {cost ?? (
                        <span className={styles.unpriced} title={UNPRICED_HINT} aria-label={UNPRICED_HINT} tabIndex={0}>
                          —
                        </span>
                      )}
                    </td>
                    <td className={`${styles.num} ${r.unpriced_requests > 0 ? styles.unpricedCount : styles.mutedNum}`}>
                      {r.unpriced_requests > 0 ? (
                        <span title={UNPRICED_HINT}>{r.unpriced_requests.toLocaleString()}</span>
                      ) : (
                        '0'
                      )}
                    </td>
                  </tr>
                )
              })
            )}
          </tbody>
        </Table>
        {!loadingTable && table && table.rows.length >= ROW_LIMIT && (
          <div className={styles.tableFoot}>
            Showing the first {ROW_LIMIT.toLocaleString()} {groupMeta.label.toLowerCase()} by estimated cost. The export holds the same rows.
          </div>
        )}
        {!loadingTable && rows.some((r) => r.unpriced_requests > 0) && (
          <div className={styles.tableFoot}>
            Unpriced requests are left out of the estimate.{' '}
            <Link to="/admin/models?tab=prices" className={styles.inlineLink}>Add a price under Prices</Link>
          </div>
        )}
      </div>

      <ReconciliationSection timeRange={timeRange} active={active} />
    </div>
  )
}
