import { useCallback, useEffect, useState } from 'react'
import { useTheme } from '../context/ThemeContext'
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import {
  getKeySummary,
  getUserSummary,
  getUsageStats,
  getUsageSummary,
  listActivity,
  listUsage,
} from '../lib/api'
import type {
  ActivityEvent,
  InferenceEvent,
  KeyUsageSummary,
  TimeRange,
  UsageBucket,
  UsageSummary,
  UserUsageSummary,
} from '../types'
import {
  eventLabel,
  eventVariant,
  formatEventDetail,
  type EventVariant,
} from '../lib/activityEvents'
import { useToast } from '../components/Toast'
import { Badge, Button, Skeleton, Table, TableEmpty } from '../components/ui'
import { ActivityDrawer } from './ActivityDrawer'
import styles from './ActivityTab.module.css'

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const PAGE_SIZE = 50

const TIME_RANGES: { label: string; value: TimeRange }[] = [
  { label: '24h', value: '24h' },
  { label: '7d',  value: '7d'  },
  { label: '30d', value: '30d' },
]

const LOG_VIEWS: { label: string; value: 'admin' | 'api' | 'all' }[] = [
  { label: 'Admin Events', value: 'admin' },
  { label: 'API Requests', value: 'api'   },
  { label: 'All',          value: 'all'   },
]

// Hex values directly since CSS vars are unreliable in SVG attributes.
// Defined per-theme because the dark grid color (#1A1E2A) is invisible on
// light mode bone backgrounds.
const CHART_COLORS = {
  dark: {
    stroke: '#5C7048',
    fill:   'rgba(92, 112, 72, 0.18)',
    grid:   '#1A1E2A',
    axis:   '#6e6460',
  },
  light: {
    stroke: '#5C7048',
    fill:   'rgba(92, 112, 72, 0.18)',
    grid:   '#d4cec4',
    axis:   '#6e6460',
  },
} as const

// ---------------------------------------------------------------------------
// Unified event type
// ---------------------------------------------------------------------------

interface UnifiedEvent {
  id: string
  created_at: string
  source: 'admin' | 'api'
  user_id: string | null
  user_display: string
  action_label: string
  action_variant: EventVariant
  detail_text: string
  ip_address: string | null
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function getTimeRange(range: TimeRange): { since: string; until: string; interval: 'hour' | 'day' } {
  const now   = new Date()
  const until = now.toISOString()
  let since: Date
  let interval: 'hour' | 'day'

  switch (range) {
    case '24h':
      since    = new Date(now.getTime() - 24 * 60 * 60 * 1000)
      interval = 'hour'
      break
    case '7d':
      since    = new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000)
      interval = 'day'
      break
    case '30d':
      since    = new Date(now.getTime() - 30 * 24 * 60 * 60 * 1000)
      interval = 'day'
      break
  }

  return { since: since.toISOString(), until, interval }
}

function formatNumber(n: number): string {
  return n.toLocaleString()
}

function formatRelativeTime(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60)  return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60)  return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24)    return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

function formatXAxisTick(bucket: string, interval: 'hour' | 'day'): string {
  const d = new Date(bucket)
  if (interval === 'hour') {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
  }
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

function pctChange(current: number, previous: number): { value: number; up: boolean } | null {
  if (previous === 0) return null
  const pct = Math.round(((current - previous) / previous) * 100)
  return { value: Math.abs(pct), up: pct >= 0 }
}

function adminEventToUnified(e: ActivityEvent): UnifiedEvent {
  return {
    id:             e.id,
    created_at:     e.created_at,
    source:         'admin',
    user_id:        e.actor_id,
    user_display:   e.actor_name ?? e.actor_email ?? e.actor_id ?? '—',
    action_label:   eventLabel(e.event_type),
    action_variant: eventVariant(e.event_type),
    detail_text:    formatEventDetail(e.detail),
    ip_address:     e.ip_address,
  }
}

// shortenId renders a UUID as an 8…4 fragment so it stays readable when no
// human-friendly name/email is available for the user.
function shortenId(id: string): string {
  return id.length > 16 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id
}

function inferenceEventToUnified(e: InferenceEvent): UnifiedEvent {
  const display = e.user_name ?? e.user_email ?? (e.user_id ? shortenId(e.user_id) : '—')
  return {
    id:             e.id,
    created_at:     e.created_at,
    source:         'api',
    user_id:        e.user_id,
    user_display:   display,
    action_label:   e.status_code < 400 ? 'Request' : 'Error',
    action_variant: e.status_code < 400 ? 'success' : 'danger',
    detail_text:    `model: ${e.model}, latency: ${e.latency_ms}ms, status: ${e.status_code}`,
    ip_address:     e.ip_address,
  }
}

// ---------------------------------------------------------------------------
// Sub-components
// ---------------------------------------------------------------------------

function ChartTooltip({ active, payload, label }: { active?: boolean; payload?: Array<{ value: number }>; label?: string }) {
  if (!active || !payload?.length) return null
  return (
    <div className={styles.tooltip}>
      <div className={styles.tooltipLabel}>{label}</div>
      <div className={styles.tooltipValue}>{formatNumber(payload[0].value)} requests</div>
    </div>
  )
}

interface SkeletonRowProps {
  cols: number
}

function SkeletonRow({ cols }: SkeletonRowProps) {
  return (
    <tr>
      {Array.from({ length: cols }).map((_, i) => (
        <td key={i}>
          <Skeleton width={`${60 + (i * 17) % 40}%`} height="14px" />
        </td>
      ))}
    </tr>
  )
}

// ---------------------------------------------------------------------------
// Main component
// ---------------------------------------------------------------------------

export function ActivityTab() {
  const { toast } = useToast()
  const { theme } = useTheme()
  const chartColors = CHART_COLORS[theme]

  const [timeRange, setTimeRange] = useState<TimeRange>('24h')
  const [logView, setLogView]     = useState<'admin' | 'api' | 'all'>('all')
  const [page, setPage]           = useState(0)

  const [summary, setSummary]         = useState<UsageSummary | null>(null)
  const [buckets, setBuckets]         = useState<UsageBucket[]>([])
  const [userSummaries, setUserSummaries] = useState<UserUsageSummary[]>([])
  const [keySummaries, setKeySummaries]   = useState<KeyUsageSummary[]>([])

  // Unified event list — holds the current page's worth of events
  const [unifiedEvents, setUnifiedEvents] = useState<UnifiedEvent[]>([])
  const [total, setTotal]                 = useState(0)

  const [loadingSummary, setLoadingSummary] = useState(true)
  const [loadingChart, setLoadingChart]     = useState(true)
  const [loadingRanks, setLoadingRanks]     = useState(true)
  const [loadingEvents, setLoadingEvents]   = useState(true)

  // Drawer state
  const [drawerUser, setDrawerUser] = useState<UserUsageSummary | null>(null)
  const [drawerKey, setDrawerKey]   = useState<KeyUsageSummary | null>(null)

  // Derived interval for chart labels
  const interval = timeRange === '24h' ? 'hour' : 'day'

  // ---------------------------------------------------------------------------
  // Data fetching
  // ---------------------------------------------------------------------------

  const fetchSummaryAndChart = useCallback(async () => {
    setLoadingSummary(true)
    setLoadingChart(true)
    setLoadingRanks(true)
    const { since, until, interval: iv } = getTimeRange(timeRange)

    try {
      const [s, b, us, ks] = await Promise.all([
        getUsageSummary({ since, until }),
        getUsageStats({ since, until, interval: iv }),
        getUserSummary({ since, until }),
        getKeySummary({ since, until }),
      ])
      setSummary(s)
      setBuckets(b)
      setUserSummaries(us.slice(0, 5))
      setKeySummaries(ks.slice(0, 5))
    } catch (err) {
      toast((err as Error).message, 'error')
      setSummary(null)
      setBuckets([])
      setUserSummaries([])
      setKeySummaries([])
    } finally {
      setLoadingSummary(false)
      setLoadingChart(false)
      setLoadingRanks(false)
    }
  }, [timeRange, toast])

  const fetchEvents = useCallback(async () => {
    setLoadingEvents(true)
    const { since, until } = getTimeRange(timeRange)
    const offset = page * PAGE_SIZE

    try {
      if (logView === 'admin') {
        const res = await listActivity({ since, until, limit: PAGE_SIZE, offset })
        setUnifiedEvents(res.events.map(adminEventToUnified))
        setTotal(res.total)
      } else if (logView === 'api') {
        const res = await listUsage({ since, until, limit: PAGE_SIZE, offset })
        setUnifiedEvents(res.events.map(inferenceEventToUnified))
        setTotal(res.total)
      } else {
        // 'all': fetch both full result sets and merge client-side
        // We fetch enough to fill two full pages worth so pagination is stable
        const fetchLimit = PAGE_SIZE * 2
        const [adminRes, apiRes] = await Promise.all([
          listActivity({ since, until, limit: fetchLimit, offset: 0 }),
          listUsage({ since, until, limit: fetchLimit, offset: 0 }),
        ])
        const merged = [
          ...adminRes.events.map(adminEventToUnified),
          ...apiRes.events.map(inferenceEventToUnified),
        ].sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())

        const totalMerged = adminRes.total + apiRes.total
        setTotal(totalMerged)
        setUnifiedEvents(merged.slice(offset, offset + PAGE_SIZE))
      }
    } catch (err) {
      toast((err as Error).message, 'error')
      setUnifiedEvents([])
      setTotal(0)
    } finally {
      setLoadingEvents(false)
    }
  }, [timeRange, logView, page, toast])

  // ---------------------------------------------------------------------------
  // Export
  // ---------------------------------------------------------------------------

  async function handleExport() {
    const { since, until } = getTimeRange(timeRange)
    const params = new URLSearchParams({ since, until })
    const path = logView === 'api' ? 'usage' : 'activity'
    try {
      const res = await fetch(`/api/v1/${path}/export?${params}`, {
        headers: { 'X-Requested-With': 'XMLHttpRequest' },
        credentials: 'same-origin',
      })
      if (!res.ok) throw new Error(`Export failed: HTTP ${res.status}`)
      const blob = await res.blob()
      const url  = URL.createObjectURL(blob)
      const a    = document.createElement('a')
      a.href     = url
      a.download = `${path}_events.csv`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }

  // ---------------------------------------------------------------------------
  // Effects
  // ---------------------------------------------------------------------------

  // Reset page when time range or log view changes
  useEffect(() => {
    setPage(0)
  }, [timeRange, logView])

  useEffect(() => {
    void fetchSummaryAndChart()
  }, [fetchSummaryAndChart])

  useEffect(() => {
    void fetchEvents()
  }, [fetchEvents])

  // ---------------------------------------------------------------------------
  // Derived values
  // ---------------------------------------------------------------------------

  const chartData = buckets.map((b) => ({
    label:    formatXAxisTick(b.bucket_time, interval),
    requests: b.request_count,
  }))

  const firstItem = page * PAGE_SIZE + 1
  const lastItem  = Math.min((page + 1) * PAGE_SIZE, total)
  const hasPrev   = page > 0
  const hasNext   = (page + 1) * PAGE_SIZE < total

  const requestChange = summary ? pctChange(summary.total_requests, summary.total_requests_previous) : null

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  return (
    <div>
      {/* Toolbar */}
      <div className={styles.toolbar}>
        <Button variant="secondary" size="sm" onClick={() => void handleExport()}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ marginRight: '6px' }}>
            <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
            <polyline points="7 10 12 15 17 10" />
            <line x1="12" y1="15" x2="12" y2="3" />
          </svg>
          Export CSV
        </Button>
        <div className={styles.timeRange}>
          {TIME_RANGES.map((r) => (
            <button
              key={r.value}
              className={`${styles.timeRangeBtn} ${timeRange === r.value ? styles.timeRangeActive : ''}`}
              onClick={() => setTimeRange(r.value)}
            >
              {r.label}
            </button>
          ))}
        </div>
      </div>

      {/* Stat cards */}
      <div className={styles.statCards}>
        {/* Total Requests */}
        <div className={styles.statCard}>
          <div className={styles.statLabel}>Total Requests</div>
          {loadingSummary ? (
            <Skeleton width="60%" height="28px" />
          ) : (
            <>
              <div className={styles.statValue}>{formatNumber(summary?.total_requests ?? 0)}</div>
              <div className={styles.statSub}>
                {requestChange !== null ? (
                  <span className={requestChange.up ? styles.statUp : styles.statDown}>
                    {requestChange.up ? '+' : '-'}{requestChange.value}% vs prev period
                  </span>
                ) : (
                  <span>vs prev period</span>
                )}
              </div>
            </>
          )}
        </div>

        {/* Active Users */}
        <div className={styles.statCard}>
          <div className={styles.statLabel}>Active Users</div>
          {loadingSummary ? (
            <Skeleton width="40%" height="28px" />
          ) : (
            <>
              <div className={styles.statValue}>{formatNumber(summary?.active_users ?? 0)}</div>
              <div className={styles.statSub}>of {formatNumber(summary?.total_users ?? 0)} total</div>
            </>
          )}
        </div>

        {/* Active API Keys */}
        <div className={styles.statCard}>
          <div className={styles.statLabel}>Active API Keys</div>
          {loadingSummary ? (
            <Skeleton width="40%" height="28px" />
          ) : (
            <>
              <div className={styles.statValue}>{formatNumber(summary?.active_api_keys ?? 0)}</div>
              <div className={styles.statSub}>of {formatNumber(summary?.total_api_keys ?? 0)} total</div>
            </>
          )}
        </div>

        {/* Avg Latency */}
        <div className={styles.statCard}>
          <div className={styles.statLabel}>Avg Latency</div>
          {loadingSummary ? (
            <Skeleton width="50%" height="28px" />
          ) : (
            <>
              <div className={styles.statValue}>{summary?.avg_latency_ms ?? 0}ms</div>
              <div className={styles.statSub}>p95: {summary?.p95_latency_ms ?? 0}ms</div>
            </>
          )}
        </div>
      </div>

      {/* Usage chart */}
      <div className={styles.chartCard}>
        <div className={styles.chartHeader}>
          <span className={styles.chartTitle}>Requests over time</span>
        </div>
        {loadingChart ? (
          <Skeleton width="100%" height="280px" borderRadius="var(--radius-md)" />
        ) : (
          <ResponsiveContainer width="100%" height={280}>
            <AreaChart data={chartData} margin={{ top: 4, right: 4, left: -16, bottom: 0 }}>
              <defs>
                <linearGradient id="requestGradient" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%"  stopColor={chartColors.stroke} stopOpacity={0.18} />
                  <stop offset="95%" stopColor={chartColors.stroke} stopOpacity={0}    />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke={chartColors.grid} vertical={false} />
              <XAxis
                dataKey="label"
                tick={{ fill: chartColors.axis, fontSize: 11 }}
                axisLine={false}
                tickLine={false}
                interval="preserveStartEnd"
              />
              <YAxis
                tick={{ fill: chartColors.axis, fontSize: 11 }}
                axisLine={false}
                tickLine={false}
                tickFormatter={(v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v))}
              />
              <Tooltip content={<ChartTooltip />} cursor={{ stroke: chartColors.grid, strokeWidth: 1 }} />
              <Area
                type="monotone"
                dataKey="requests"
                stroke={chartColors.stroke}
                strokeWidth={2}
                fill={chartColors.fill}
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>

      {/* Top Users / Top API Keys */}
      <div className={styles.rankGrid}>
        {/* Top Users */}
        <div className={styles.rankCard}>
          <div className={styles.rankCardHeader}>Top Users</div>
          {loadingRanks ? (
            Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className={styles.rankRow}>
                <Skeleton width="100%" height="14px" />
              </div>
            ))
          ) : userSummaries.length === 0 ? (
            <div className={styles.rankEmpty}>No data for this period</div>
          ) : (
            userSummaries.map((u, i) => (
              <div key={u.user_id} className={styles.rankRow}>
                <span className={styles.rankNum}>{i + 1}</span>
                <button
                  className={styles.rankNameBtn}
                  onClick={() => { setDrawerUser(u); setDrawerKey(null) }}
                  title={u.user_email ?? u.user_id}
                >
                  {u.user_name ?? u.user_email ?? u.user_id}
                </button>
                <span className={styles.rankMeta}>
                  {u.request_count.toLocaleString()} req · {Math.round(u.avg_latency_ms)}ms
                </span>
              </div>
            ))
          )}
        </div>

        {/* Top API Keys */}
        <div className={styles.rankCard}>
          <div className={styles.rankCardHeader}>Top API Keys</div>
          {loadingRanks ? (
            Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className={styles.rankRow}>
                <Skeleton width="100%" height="14px" />
              </div>
            ))
          ) : keySummaries.length === 0 ? (
            <div className={styles.rankEmpty}>No data for this period</div>
          ) : (
            keySummaries.map((k, i) => (
              <div key={k.api_key_id} className={styles.rankRow}>
                <span className={styles.rankNum}>{i + 1}</span>
                <button
                  className={styles.rankNameBtn}
                  onClick={() => { setDrawerKey(k); setDrawerUser(null) }}
                  title={k.key_prefix ?? k.api_key_id}
                >
                  {k.key_name ?? k.key_prefix ?? k.api_key_id}
                </button>
                <span className={styles.rankMeta}>
                  {k.request_count.toLocaleString()} req · {Math.round(k.avg_latency_ms)}ms
                </span>
              </div>
            ))
          )}
        </div>
      </div>

      {/* Activity log */}
      <div className={styles.logHeader}>
        <div className={styles.logToggle}>
          {LOG_VIEWS.map((v) => (
            <button
              key={v.value}
              className={`${styles.logToggleBtn} ${logView === v.value ? styles.logToggleActive : ''}`}
              onClick={() => setLogView(v.value)}
            >
              {v.label}
            </button>
          ))}
        </div>
      </div>

      <div className={styles.tableCard}>
        <Table>
          <thead>
            <tr>
              <th>Time</th>
              <th>User</th>
              <th>Action</th>
              <th>Details</th>
              <th>IP</th>
            </tr>
          </thead>
          <tbody>
            {loadingEvents ? (
              Array.from({ length: 8 }).map((_, i) => <SkeletonRow key={i} cols={5} />)
            ) : unifiedEvents.length === 0 ? (
              <TableEmpty colSpan={5}>
                <div className={styles.empty}>
                  <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                    <line x1="18" y1="20" x2="18" y2="10" />
                    <line x1="12" y1="20" x2="12" y2="4" />
                    <line x1="6" y1="20" x2="6" y2="14" />
                  </svg>
                  <span className={styles.emptyTitle}>No activity yet</span>
                  <span className={styles.emptyHint}>Events will appear here once users start interacting with the system</span>
                </div>
              </TableEmpty>
            ) : (
              unifiedEvents.map((e) => {
                // Find matching user summary for clickable user cells
                const matchedUser = e.source === 'api' && e.user_id
                  ? userSummaries.find((u) => u.user_id === e.user_id) ?? null
                  : null

                return (
                  <tr key={e.id}>
                    <td className={styles.date} title={new Date(e.created_at).toISOString()}>
                      {formatRelativeTime(e.created_at)}
                    </td>
                    <td className={styles.muted}>
                      {matchedUser ? (
                        <button
                          style={{ background: 'none', border: 'none', padding: 0, cursor: 'pointer', color: 'var(--fg-muted)', fontSize: 'inherit', textDecoration: 'underline dotted' }}
                          onClick={() => { setDrawerUser(matchedUser); setDrawerKey(null) }}
                        >
                          {e.user_display}
                        </button>
                      ) : (
                        e.user_display
                      )}
                    </td>
                    <td>
                      <Badge variant={e.action_variant}>{e.action_label}</Badge>
                    </td>
                    <td>
                      <span className={styles.detailText} title={e.detail_text}>
                        {e.detail_text}
                      </span>
                    </td>
                    <td className={styles.muted}>{e.ip_address ?? '—'}</td>
                  </tr>
                )
              })
            )}
          </tbody>
        </Table>

        {/* Pagination — only shown when there's data */}
        {!loadingEvents && total > 0 && (
          <div className={styles.pagination}>
            <span className={styles.pageInfo}>
              Showing {firstItem}–{lastItem} of {formatNumber(total)} events
            </span>
            <div className={styles.pageButtons}>
              <Button variant="secondary" size="sm" disabled={!hasPrev} onClick={() => setPage((p) => p - 1)}>
                Prev
              </Button>
              <Button variant="secondary" size="sm" disabled={!hasNext} onClick={() => setPage((p) => p + 1)}>
                Next
              </Button>
            </div>
          </div>
        )}
      </div>

      {/* Drill-down drawer */}
      <ActivityDrawer
        open={drawerUser !== null || drawerKey !== null}
        onClose={() => { setDrawerUser(null); setDrawerKey(null) }}
        user={drawerUser}
        apiKey={drawerKey}
        timeRange={timeRange}
      />
    </div>
  )
}
