import { useCallback, useEffect, useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import * as VisuallyHidden from '@radix-ui/react-visually-hidden'
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { getUsageStats, listActivity, listUsage } from '../lib/api'
import { useTheme } from '../context/ThemeContext'
import type { InferenceEvent, KeyUsageSummary, TimeRange, UsageBucket, UserUsageSummary } from '../types'
import { eventLabel, eventVariant, formatEventDetail, type EventVariant } from '../lib/activityEvents'
import { denialLabel, routerReasonText } from '../lib/modelRouting'
import { useToast } from '../components/Toast'
import { Avatar, Badge, Skeleton } from '../components/ui'
import styles from './ActivityDrawer.module.css'

const CHART_COLORS = {
  dark:  { stroke: '#5C7048', grid: '#1A1E2A', axis: '#6e6460' },
  light: { stroke: '#5C7048', grid: '#d4cec4', axis: '#6e6460' },
} as const

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

/** One recorded call as a drawer row. A classifier call is named for the job it
 *  did — it chose a route, it did not answer anyone — and carries the route it
 *  chose so the row explains itself without a second lookup. */
function inferenceToRow(e: InferenceEvent): {
  id: string
  created_at: string
  label: string
  variant: EventVariant
  detail: string
} {
  const purpose = e.purpose ?? 'inference'
  const label =
    e.status_code >= 400
      ? e.denial_code
        ? 'Refused'
        : 'Error'
      : purpose === 'routing'
        ? 'Classifier'
        : purpose === 'routing_dryrun'
          ? 'Route test'
          : 'Request'
  const detail = [
    e.denial_code ? `${denialLabel(e.denial_code)} (${e.denial_code})` : '',
    `model: ${e.model}`,
    e.route_name ? `route: ${e.route_name}` : '',
    e.router_reason ? routerReasonText(e.router_reason).toLowerCase() : '',
    `${e.latency_ms}ms`,
    `status: ${e.status_code}`,
  ]
    .filter(Boolean)
    .join(', ')
  return {
    id: e.id,
    created_at: e.created_at,
    label,
    variant: e.status_code >= 400 ? 'danger' : purpose === 'inference' ? 'success' : 'info',
    detail,
  }
}

// ---------------------------------------------------------------------------
// Mini chart tooltip
// ---------------------------------------------------------------------------

function MiniChartTooltip({ active, payload, label }: { active?: boolean; payload?: Array<{ value: number }>; label?: string }) {
  if (!active || !payload?.length) return null
  return (
    <div className={styles.miniTooltip}>
      <div className={styles.miniTooltipLabel}>{label}</div>
      <div className={styles.miniTooltipValue}>
        {payload[0].value} requests
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

interface DrawerProps {
  open: boolean
  onClose: () => void
  user?: UserUsageSummary | null
  apiKey?: KeyUsageSummary | null
  timeRange: TimeRange
}

// ---------------------------------------------------------------------------
// ActivityDrawer
// ---------------------------------------------------------------------------

export function ActivityDrawer({ open, onClose, user, apiKey, timeRange }: DrawerProps) {
  const { toast } = useToast()
  const { theme } = useTheme()
  const chartColors = CHART_COLORS[theme]

  const [buckets, setBuckets] = useState<UsageBucket[]>([])
  const [recentEvents, setRecentEvents] = useState<Array<{ id: string; created_at: string; label: string; variant: 'neutral' | 'info' | 'warning' | 'danger' | 'success'; detail: string }>>([])
  const [loadingChart, setLoadingChart] = useState(false)
  const [loadingEvents, setLoadingEvents] = useState(false)

  const interval = timeRange === '24h' ? 'hour' : 'day'

  const fetchDrawerData = useCallback(async () => {
    if (!open) return
    if (!user && !apiKey) return

    const { since, until, interval: iv } = getTimeRange(timeRange)

    // Fetch chart buckets
    setLoadingChart(true)
    try {
      const statsParams: Parameters<typeof getUsageStats>[0] = { since, until, interval: iv }
      if (user)   statsParams.user_id    = user.user_id
      if (apiKey) statsParams.api_key_id = apiKey.api_key_id
      const b = await getUsageStats(statsParams)
      setBuckets(b)
    } catch (err) {
      toast((err as Error).message, 'error')
      setBuckets([])
    } finally {
      setLoadingChart(false)
    }

    // Fetch recent events
    setLoadingEvents(true)
    try {
      if (user) {
        // For users: fetch both admin events (by actor_id) and inference events, merge, take 10
        const [activityRes, usageRes] = await Promise.all([
          listActivity({ actor_id: user.user_id, since, until, limit: 10, offset: 0 }),
          listUsage({ user_id: user.user_id, since, until, limit: 10, offset: 0 }),
        ])

        const adminMapped = activityRes.events.map((e) => ({
          id: e.id,
          created_at: e.created_at,
          label: eventLabel(e.event_type),
          variant: eventVariant(e.event_type),
          detail: e.detail ? formatEventDetail(e.detail) : '',
        }))

        const inferenceMapped = usageRes.events.map(inferenceToRow)

        const merged = [...adminMapped, ...inferenceMapped]
          .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
          .slice(0, 10)
        setRecentEvents(merged)
      } else if (apiKey) {
        const usageRes = await listUsage({ api_key_id: apiKey.api_key_id, since, until, limit: 10, offset: 0 })
        setRecentEvents(usageRes.events.map(inferenceToRow))
      }
    } catch (err) {
      toast((err as Error).message, 'error')
      setRecentEvents([])
    } finally {
      setLoadingEvents(false)
    }
  }, [open, user, apiKey, timeRange, toast])

  useEffect(() => {
    if (open) {
      void fetchDrawerData()
    } else {
      // Reset state when drawer closes so stale data doesn't flash on re-open
      setBuckets([])
      setRecentEvents([])
    }
  }, [open, fetchDrawerData])

  const chartData = buckets.map((b) => ({
    label:    formatXAxisTick(b.bucket_time, interval),
    requests: b.request_count,
  }))

  // Derive a title for the accessible dialog title
  const dialogTitle = user
    ? (user.user_name ?? user.user_email ?? user.user_id)
    : apiKey
      ? (apiKey.key_name ?? apiKey.key_prefix ?? 'API Key')
      : 'Details'

  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.overlay} />
        <Dialog.Content
          className={styles.drawer}
          aria-describedby={undefined}
        >
          <VisuallyHidden.Root asChild>
            <Dialog.Title>{dialogTitle}</Dialog.Title>
          </VisuallyHidden.Root>
          <VisuallyHidden.Root asChild>
            <Dialog.Description>Activity details for {dialogTitle}</Dialog.Description>
          </VisuallyHidden.Root>

          <Dialog.Close asChild>
            <button className={styles.closeBtn} aria-label="Close">
              <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
                <line x1="3" y1="3" x2="11" y2="11" />
                <line x1="11" y1="3" x2="3" y2="11" />
              </svg>
            </button>
          </Dialog.Close>

          {/* Header */}
          {user && (
            <div className={styles.header}>
              <Avatar name={user.user_name ?? user.user_email ?? '?'} size="md" />
              <div className={styles.headerInfo}>
                <div className={styles.name}>{user.user_name ?? '—'}</div>
                <div className={styles.subtitle}>{user.user_email ?? user.user_id}</div>
              </div>
            </div>
          )}

          {apiKey && (
            <div className={styles.header}>
              <div className={styles.keyIcon}>
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <circle cx="7.5" cy="15.5" r="5.5" />
                  <path d="M21 2l-9.6 9.6" />
                  <path d="M15.5 7.5l3 3L22 7l-3-3" />
                </svg>
              </div>
              <div className={styles.headerInfo}>
                <div className={styles.name}>{apiKey.key_name ?? 'Unnamed key'}</div>
                <div className={styles.subtitleMono}>
                  {apiKey.key_prefix ?? '—'}
                  {apiKey.owner_name ? ` · ${apiKey.owner_name}` : ''}
                </div>
              </div>
            </div>
          )}

          {/* Stats row */}
          <div className={styles.statsRow}>
            <div>
              <div className={styles.miniStatLabel}>Requests</div>
              <div className={styles.miniStatValue}>
                {(user?.request_count ?? apiKey?.request_count ?? 0).toLocaleString()}
              </div>
            </div>
            <div>
              <div className={styles.miniStatLabel}>Avg Latency</div>
              <div className={styles.miniStatValue}>
                {Math.round(user?.avg_latency_ms ?? apiKey?.avg_latency_ms ?? 0)}ms
              </div>
            </div>
            <div>
              <div className={styles.miniStatLabel}>Last Active</div>
              <div className={styles.miniStatValue}>
                {user?.last_active
                  ? formatRelativeTime(user.last_active)
                  : apiKey?.last_active
                    ? formatRelativeTime(apiKey.last_active)
                    : '—'}
              </div>
            </div>
          </div>

          {/* Mini chart */}
          <div className={styles.section}>
            <div className={styles.sectionTitle}>Requests over time</div>
            {loadingChart ? (
              <Skeleton width="100%" height="120px" />
            ) : (
              <ResponsiveContainer width="100%" height={120}>
                <AreaChart data={chartData} margin={{ top: 4, right: 4, left: -28, bottom: 0 }}>
                  <defs>
                    <linearGradient id="drawerGradient" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%"  stopColor={chartColors.stroke} stopOpacity={0.18} />
                      <stop offset="95%" stopColor={chartColors.stroke} stopOpacity={0}    />
                    </linearGradient>
                  </defs>
                  <CartesianGrid strokeDasharray="3 3" stroke={chartColors.grid} vertical={false} />
                  <XAxis
                    dataKey="label"
                    tick={{ fill: chartColors.axis, fontSize: 10 }}
                    axisLine={false}
                    tickLine={false}
                    interval="preserveStartEnd"
                  />
                  <YAxis
                    tick={{ fill: chartColors.axis, fontSize: 10 }}
                    axisLine={false}
                    tickLine={false}
                    tickFormatter={(v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v))}
                  />
                  <Tooltip content={<MiniChartTooltip />} cursor={{ stroke: chartColors.grid, strokeWidth: 1 }} />
                  <Area
                    type="monotone"
                    dataKey="requests"
                    stroke={chartColors.stroke}
                    strokeWidth={1.5}
                    fill="url(#drawerGradient)"
                  />
                </AreaChart>
              </ResponsiveContainer>
            )}
          </div>

          {/* Recent events */}
          <div className={styles.section}>
            <div className={styles.sectionTitle}>Recent events</div>
            {loadingEvents ? (
              <div className={styles.skeletonList}>
                {Array.from({ length: 5 }).map((_, i) => (
                  <Skeleton key={i} width={`${70 + (i * 11) % 25}%`} height="14px" />
                ))}
              </div>
            ) : recentEvents.length === 0 ? (
              <div className={styles.emptyEvents}>
                No events in this time range
              </div>
            ) : (
              <div className={styles.eventList}>
                {recentEvents.map((e) => (
                  <div key={e.id} className={styles.eventItem}>
                    <span className={styles.eventTime}>{formatRelativeTime(e.created_at)}</span>
                    <Badge variant={e.variant}>{e.label}</Badge>
                    {e.detail && (
                      <span className={styles.eventDetail} title={e.detail}>{e.detail}</span>
                    )}
                  </div>
                ))}
              </div>
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
