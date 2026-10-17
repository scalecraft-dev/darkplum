import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, getReconciliation, refreshReconciliation } from '../lib/api'
import {
  BEDROCK_RECONCILIATION_NOTE,
  RECONCILIATION_DELTA_NOTE,
  RECONCILIATION_GROUPS,
  RECONCILIATION_VENDORS,
  billingStateCopy,
  formatDelta,
  lastPulledText,
  PULL_COOLDOWN_MS,
  pullCooldownSeconds,
} from '../lib/billing'
import { formatRetryCountdown, providerLabel } from '../lib/modelRouting'
import { formatCost } from '../lib/spend'
import type {
  ReconciliationGroupBy,
  ReconciliationReport,
  ReconciliationVendorStatus,
  TimeRange,
} from '../types'
import { useToast } from '../components/Toast'
import { Badge, Button, CopyButton, Skeleton, Table, TableEmpty } from '../components/ui'
import activityStyles from './ActivityTab.module.css'
import spendStyles from './SpendView.module.css'
import styles from './ReconciliationSection.module.css'

/**
 * Activity > Spend > Reconciliation: Darkplum's list-price estimate beside
 * what each vendor's own usage and cost report says.
 *
 * The vendor side needs a billing key (Anthropic, OpenAI) — a separate,
 * broader credential installed on Admin > Models — or, for Bedrock, the api's
 * AWS role. Without any of those there is nothing to compare, and the empty
 * state says what the comparison would add and where to turn it on.
 */

/** While a pull is running the panel re-reads itself this often. */
const PULLING_POLL_MS = 5_000

function rangeIso(range: TimeRange): { since: string; until: string } {
  const until = Date.now()
  const hours = range === '24h' ? 24 : range === '7d' ? 24 * 7 : 24 * 30
  return { since: new Date(until - hours * 3_600_000).toISOString(), until: new Date(until).toISOString() }
}

function vendorStatuses(report: ReconciliationReport | null): ReconciliationVendorStatus[] {
  return RECONCILIATION_VENDORS.map(
    (vendor) =>
      report?.vendors.find((v) => v.vendor === vendor) ?? {
        vendor,
        configured: false,
        state: 'not_configured' as const,
        last_pulled_at: null,
        key_last4: null,
      },
  )
}

export function ReconciliationSection({ timeRange, active }: { timeRange: TimeRange; active: boolean }) {
  const { toast } = useToast()
  const [group, setGroup] = useState<ReconciliationGroupBy>('day')
  const [report, setReport] = useState<ReconciliationReport | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [loadedKey, setLoadedKey] = useState<string | null>(null)

  const [pressedAt, setPressedAt] = useState<number | null>(null)
  /** A 429's own interval replaces the 60-second window when it is longer. */
  const [cooldownMs, setCooldownMs] = useState(PULL_COOLDOWN_MS)
  const [posting, setPosting] = useState(false)
  const [now, setNow] = useState(() => Date.now())
  const cooldown = pullCooldownSeconds(pressedAt, now, cooldownMs)

  const load = useCallback(
    async (quiet = false) => {
      if (!quiet) setLoading(true)
      try {
        setReport(await getReconciliation({ ...rangeIso(timeRange), group_by: group }))
        setError(null)
      } catch (err) {
        if (!quiet) setReport(null)
        setError((err as Error).message)
      } finally {
        if (!quiet) setLoading(false)
      }
    },
    [timeRange, group],
  )

  useEffect(() => {
    const k = `${timeRange}:${group}`
    if (!active || loadedKey === k) return
    setLoadedKey(k)
    void load()
  }, [active, timeRange, group, loadedKey, load])

  const vendors = vendorStatuses(report)
  const anyPulling = vendors.some((v) => v.state === 'pulling')
  const anyConfigured = vendors.some((v) => v.configured)

  // Re-read while a pull is running, so the chips settle on their own.
  useEffect(() => {
    if (!active || !anyPulling) return
    const id = setInterval(() => void load(true), PULLING_POLL_MS)
    return () => clearInterval(id)
  }, [active, anyPulling, load])

  // The countdown's second hand, only while there is a countdown.
  useEffect(() => {
    if (cooldown <= 0) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [cooldown])

  async function pullNow() {
    setPosting(true)
    try {
      await refreshReconciliation()
      setCooldownMs(PULL_COOLDOWN_MS)
      setPressedAt(Date.now())
      setNow(Date.now())
      toast('Pulling vendor reports. This panel updates when they land.', 'success')
      void load(true)
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) {
        setCooldownMs(Math.max(PULL_COOLDOWN_MS, (err.retryAfterSeconds ?? 60) * 1000))
        setPressedAt(Date.now())
        setNow(Date.now())
      }
      toast((err as Error).message, 'error')
    } finally {
      setPosting(false)
    }
  }

  const groupMeta = RECONCILIATION_GROUPS.find((g) => g.value === group) ?? RECONCILIATION_GROUPS[0]
  const showVendorCol = group !== 'vendor'
  const cols = showVendorCol ? 6 : 5
  const rows = report?.rows ?? []

  return (
    <section className={styles.section} aria-labelledby="reconciliation-heading">
      <div className={styles.head}>
        <div>
          <h2 id="reconciliation-heading" className={styles.title}>Reconciliation</h2>
          <p className={styles.subtitle}>
            Darkplum's estimate beside each vendor's own usage and cost report, pulled once a day.
          </p>
        </div>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void pullNow()}
          disabled={!anyConfigured || posting || cooldown > 0 || anyPulling}
          title={!anyConfigured ? 'No vendor has a billing key or AWS role to pull with' : undefined}
        >
          {posting || anyPulling
            ? 'Pulling…'
            : cooldown > 0
              ? `Pull again in ${formatRetryCountdown(cooldown)}`
              : 'Pull now'}
        </Button>
      </div>

      {/* One chip per vendor, always all three: a vendor that is missing from
          the list cannot be used to find out it is not set up. */}
      <ul className={styles.chips} aria-label="Vendor reports">
        {vendors.map((v) => {
          const copy = billingStateCopy(v.vendor, v.state)
          return (
            <li key={v.vendor} className={styles.chip}>
              <div className={styles.chipHead}>
                <span className={styles.chipName}>{providerLabel(v.vendor)}</span>
                {loading && !report ? (
                  <Skeleton width="72px" height="18px" />
                ) : (
                  <Badge variant={copy.variant} bordered dashed={copy.dashed}>{copy.label}</Badge>
                )}
              </div>
              {!(loading && !report) && (
                <>
                  <p className={styles.chipMeta}>
                    {v.vendor === 'bedrock' && 'Via AWS Cost Explorer · '}
                    {lastPulledText(v.last_pulled_at)}
                    {v.key_last4 && (
                      <>
                        {' · key ending '}
                        <code className={styles.mono}>{v.key_last4}</code>
                      </>
                    )}
                  </p>
                  {v.state !== 'ok' && <p className={styles.chipDetail}>{copy.detail}</p>}
                </>
              )}
            </li>
          )
        })}
      </ul>

      <p className={styles.note}>{BEDROCK_RECONCILIATION_NOTE}</p>

      {error && !loading ? (
        <div className={`${activityStyles.tableCard} ${styles.placeholder}`} role="status">
          <span>Reconciliation could not be loaded: {error}</span>
          <Button variant="secondary" size="sm" onClick={() => setLoadedKey(null)}>Try again</Button>
        </div>
      ) : !loading && !anyConfigured ? (
        <div className={`${activityStyles.tableCard} ${styles.placeholder}`}>
          <div className={activityStyles.empty}>
            <span className={activityStyles.emptyTitle}>No vendor reports to compare against yet</span>
            <span className={`${activityStyles.emptyHint} ${styles.emptyHint}`}>
              With a billing key installed, this table shows what Anthropic or OpenAI actually
              reported for each day and model beside Darkplum's estimate — the check that catches a
              missing price, a contract rate, or usage that never went through Darkplum.
            </span>
            <Link to="/admin/models" className={spendStyles.inlineLink}>
              Install a billing key under Admin &gt; Models
            </Link>
          </div>
        </div>
      ) : (
        <>
          <p className={styles.note}>{RECONCILIATION_DELTA_NOTE}</p>
          <div className={spendStyles.tableHeader}>
            <div className={activityStyles.logToggle} role="group" aria-label="Group reconciliation by">
              {RECONCILIATION_GROUPS.map((g) => (
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
          </div>

          <div className={activityStyles.tableCard}>
            <Table label={`Estimated against vendor-reported spend by ${groupMeta.column.toLowerCase()}`}>
              <thead>
                <tr>
                  <th>{groupMeta.column}</th>
                  {showVendorCol && <th>Vendor</th>}
                  <th className={spendStyles.num}>Estimated</th>
                  <th className={spendStyles.num}>Vendor-reported</th>
                  <th className={spendStyles.num}>Difference</th>
                  <th className={spendStyles.num}>Reported tokens (in / cached / out)</th>
                </tr>
              </thead>
              <tbody>
                {loading ? (
                  Array.from({ length: 4 }).map((_, i) => (
                    <tr key={i}>
                      {Array.from({ length: cols }).map((__, j) => (
                        <td key={j}><Skeleton width={`${50 + ((i + j) * 13) % 40}%`} height="14px" /></td>
                      ))}
                    </tr>
                  ))
                ) : rows.length === 0 ? (
                  <TableEmpty colSpan={cols}>
                    <div className={activityStyles.empty}>
                      <span className={activityStyles.emptyTitle}>Nothing reported for this period yet</span>
                      <span className={activityStyles.emptyHint}>
                        Vendor reports lag by up to a day. Widen the time range, or pull now.
                      </span>
                    </div>
                  </TableEmpty>
                ) : (
                  rows.map((r, i) => {
                    const delta = formatDelta(r.delta_cents)
                    const est = formatCost(r.estimated_cents)
                    const rep = formatCost(r.reported_cents)
                    const isModel = group === 'model' && !!r.key
                    return (
                      <tr key={`${r.vendor}-${r.key}-${i}`}>
                        <td>
                          <div className={spendStyles.labelCell}>
                            {isModel ? (
                              <>
                                <code className={spendStyles.mono}>{r.label || r.key}</code>
                                <CopyButton value={r.key} label="Copy model" />
                              </>
                            ) : (
                              <span>{group === 'vendor' ? providerLabel(r.vendor) : r.label || r.key}</span>
                            )}
                          </div>
                        </td>
                        {showVendorCol && <td>{providerLabel(r.vendor)}</td>}
                        <td className={spendStyles.num}>{est ?? <span className={spendStyles.mutedNum}>—</span>}</td>
                        <td className={spendStyles.num}>
                          {rep ?? (
                            <span className={spendStyles.mutedNum} title="Not in the vendor's report (yet)">—</span>
                          )}
                        </td>
                        <td className={spendStyles.num}>
                          {delta ? (
                            <span className={`${styles.delta} ${styles[delta.direction]}`}>
                              <span aria-hidden="true" className={styles.deltaGlyph}>{delta.glyph}</span>
                              <span aria-hidden="true">{delta.text}</span>
                              <span className={styles.srOnly}>{delta.spoken}</span>
                            </span>
                          ) : (
                            <span className={spendStyles.mutedNum}>—</span>
                          )}
                        </td>
                        <td className={`${spendStyles.num} ${r.reported_tokens ? '' : spendStyles.mutedNum}`}>
                          {r.reported_tokens
                            ? `${r.reported_tokens.input.toLocaleString()} / ${r.reported_tokens.cached.toLocaleString()} / ${r.reported_tokens.output.toLocaleString()}`
                            : '—'}
                        </td>
                      </tr>
                    )
                  })
                )}
              </tbody>
            </Table>
          </div>
        </>
      )}
    </section>
  )
}
