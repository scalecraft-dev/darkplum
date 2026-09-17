import { useCallback, useEffect, useMemo, useState } from 'react'
import { listAdminModels, toggleModel } from '../lib/api'
import { formatModelName } from '../lib/formatModelName'
import type { AdminModel } from '../types'
import { useToast } from '../components/Toast'
import { Badge, Table, TableEmpty } from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import { Skeleton } from '../components/ui/Skeleton'
import pageStyles from './PageLayout.module.css'
import styles from './ModelsPage.module.css'

type Filter = 'all' | 'enabled' | 'disabled'

const PROVIDER_LABELS: Record<string, string> = {
  bedrock: 'AWS Bedrock',
  vllm: 'Local',
  lmstudio: 'LM Studio',
}

function providerLabel(provider: string): string {
  return PROVIDER_LABELS[provider] ?? provider
}

/** Extract the vendor from a Bedrock inference profile ID.
 *  "us.anthropic.claude-3-5-sonnet-..." → "anthropic"
 *  "global.amazon.nova-pro-v1:0"       → "amazon"
 *  "anthropic.claude-..."              → "anthropic"  (foundation model fallback)
 */
function extractVendor(modelId: string): string {
  const parts = modelId.split('.')
  // Inference profile IDs: region.vendor.model (3+ segments)
  if (parts.length >= 3) return parts[1]
  // Foundation model IDs: vendor.model (2 segments)
  if (parts.length === 2) return parts[0]
  return ''
}

function vendorLabel(vendor: string): string {
  if (!vendor) return 'Other'
  return vendor.charAt(0).toUpperCase() + vendor.slice(1)
}

// ---------------------------------------------------------------------------
// Toggle switch component
// ---------------------------------------------------------------------------

function ToggleSwitch({
  enabled,
  loading,
  onToggle,
  modelName,
}: {
  enabled: boolean
  loading: boolean
  onToggle: () => void
  modelName: string
}) {
  return (
    <button
      className={`${styles.toggle} ${enabled ? styles.toggleEnabled : ''}`}
      onClick={onToggle}
      disabled={loading}
      role="switch"
      aria-checked={enabled}
      aria-label={`${enabled ? 'Disable' : 'Enable'} ${modelName}`}
    >
      {loading ? (
        <svg
          width="12"
          height="12"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.5"
          className={styles.toggleSpinner}
          aria-hidden="true"
        >
          <path d="M21 12a9 9 0 1 1-6.219-8.56" />
        </svg>
      ) : (
        <span className={styles.toggleKnob} />
      )}
    </button>
  )
}

// ---------------------------------------------------------------------------
// Provider section (collapsible card with table)
// ---------------------------------------------------------------------------

interface ProviderSectionProps {
  provider: string
  models: AdminModel[]
  togglingIds: Set<string>
  onToggle: (model: AdminModel) => void
}

function ProviderSection({ provider, models, togglingIds, onToggle }: ProviderSectionProps) {
  const [collapsed, setCollapsed] = useState(false)

  const enabledCount = models.filter((m) => m.enabled).length

  // For Bedrock, sub-group by vendor
  const isBedrock = provider === 'bedrock'
  const vendorGroups = useMemo(() => {
    if (!isBedrock) return null
    const groups = new Map<string, AdminModel[]>()
    for (const m of models) {
      const vendor = extractVendor(m.model_id)
      const key = vendor || '_other'
      if (!groups.has(key)) groups.set(key, [])
      groups.get(key)!.push(m)
    }
    // Sort vendor groups alphabetically
    return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [isBedrock, models])

  const renderModelRow = (m: AdminModel) => {
    const displayName = m.display_name ?? formatModelName(m.model_id)
    return (
      <tr key={m.id}>
        <td>
          <div className={styles.modelCell}>
            <span className={styles.modelName}>{displayName}</span>
          </div>
        </td>
        <td>
          <div className={styles.modelIdCell}>
            <code className={styles.modelId}>{m.model_id}</code>
            <CopyButton value={m.model_id} label="Copy" />
          </div>
        </td>
        <td>
          <Badge variant={m.enabled ? 'success' : 'neutral'}>
            {m.enabled ? 'Enabled' : 'Disabled'}
          </Badge>
        </td>
        <td className={styles.statusCol}>
          <ToggleSwitch
            enabled={m.enabled}
            loading={togglingIds.has(m.id)}
            onToggle={() => onToggle(m)}
            modelName={displayName}
          />
        </td>
      </tr>
    )
  }

  return (
    <div className={styles.providerSection}>
      <div className={styles.providerCard}>
        <div
          className={styles.providerHeader}
          onClick={() => setCollapsed((prev) => !prev)}
          role="button"
          tabIndex={0}
          aria-expanded={!collapsed}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              setCollapsed((prev) => !prev)
            }
          }}
        >
          <div className={styles.providerHeaderLeft}>
            <span className={styles.providerName}>{providerLabel(provider)}</span>
            <span className={styles.providerCount}>
              {enabledCount} of {models.length} enabled
            </span>
          </div>
          <svg
            className={`${styles.collapseChevron} ${!collapsed ? styles.collapseChevronOpen : ''}`}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <polyline points="6 9 12 15 18 9" />
          </svg>
        </div>

        <div className={collapsed ? styles.providerBodyCollapsed : styles.providerBody}>
          {vendorGroups ? (
            // Bedrock: render sub-groups with vendor headings
            vendorGroups.map(([vendor, vendorModels]) => (
              <div key={vendor}>
                <div className={styles.vendorHeading}>{vendorLabel(vendor)}</div>
                <Table>
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th>Model ID</th>
                      <th>Status</th>
                      <th className={styles.statusCol}></th>
                    </tr>
                  </thead>
                  <tbody>
                    {vendorModels.map(renderModelRow)}
                  </tbody>
                </Table>
              </div>
            ))
          ) : (
            // Non-bedrock: flat table
            <Table>
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Status</th>
                  <th className={styles.statusCol}></th>
                </tr>
              </thead>
              <tbody>
                {models.map(renderModelRow)}
              </tbody>
            </Table>
          )}
        </div>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

function LoadingSkeleton() {
  return (
    <div className={styles.providerSection}>
      <div className={styles.providerCard}>
        <div className={styles.providerHeader} style={{ cursor: 'default' }}>
          <div className={styles.providerHeaderLeft}>
            <Skeleton width="120px" height="16px" />
            <Skeleton width="100px" height="18px" borderRadius="var(--radius-full)" />
          </div>
        </div>
        <div className={styles.providerBody}>
          {[1, 2, 3].map((i) => (
            <div key={i} className={styles.skeletonRow}>
              <Skeleton width="160px" height="14px" />
              <Skeleton width="240px" height="14px" />
              <Skeleton width="60px" height="18px" borderRadius="var(--radius-full)" />
              <Skeleton width="36px" height="20px" borderRadius="var(--radius-full)" />
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Main page
// ---------------------------------------------------------------------------

export function ModelsPage() {
  const { toast } = useToast()
  const [models, setModels] = useState<AdminModel[]>([])
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [togglingIds, setTogglingIds] = useState<Set<string>>(new Set())

  const loadModels = useCallback(async () => {
    try {
      const data = await listAdminModels()
      setModels(data)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }, [toast])

  useEffect(() => {
    void loadModels()
  }, [loadModels])

  const handleToggle = async (model: AdminModel) => {
    const newEnabled = !model.enabled
    setTogglingIds((prev) => new Set(prev).add(model.id))

    try {
      const updated = await toggleModel(model.id, newEnabled)
      setModels((prev) =>
        prev.map((m) => (m.id === updated.id ? updated : m)),
      )
      toast(
        `${model.display_name ?? formatModelName(model.model_id)} ${newEnabled ? 'enabled' : 'disabled'}`,
        'success',
      )
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setTogglingIds((prev) => {
        const next = new Set(prev)
        next.delete(model.id)
        return next
      })
    }
  }

  // Filter + search
  const filtered = useMemo(() => {
    let result = models

    if (filter === 'enabled') result = result.filter((m) => m.enabled)
    if (filter === 'disabled') result = result.filter((m) => !m.enabled)

    if (search.trim()) {
      const q = search.trim().toLowerCase()
      result = result.filter(
        (m) =>
          m.model_id.toLowerCase().includes(q) ||
          (m.display_name?.toLowerCase().includes(q) ?? false) ||
          formatModelName(m.model_id).toLowerCase().includes(q),
      )
    }

    return result
  }, [models, filter, search])

  // Group by provider
  const providerGroups = useMemo(() => {
    const groups = new Map<string, AdminModel[]>()
    for (const m of filtered) {
      if (!groups.has(m.provider)) groups.set(m.provider, [])
      groups.get(m.provider)!.push(m)
    }
    // Sort: vllm (local) first, then bedrock, then others
    const order = ['vllm', 'bedrock']
    return [...groups.entries()].sort(([a], [b]) => {
      const ai = order.indexOf(a)
      const bi = order.indexOf(b)
      if (ai === -1 && bi === -1) return a.localeCompare(b)
      if (ai === -1) return 1
      if (bi === -1) return -1
      return ai - bi
    })
  }, [filtered])

  const totalEnabled = models.filter((m) => m.enabled).length

  return (
    <div className={pageStyles.page}>
      <div className={pageStyles.header}>
        <h1 className={pageStyles.title}>Models</h1>
        <p className={pageStyles.subtitle}>Enable models for your organization</p>
      </div>

      {/* Search + filters */}
      <div className={styles.toolbar}>
        <div className={styles.searchWrap}>
          <svg
            className={styles.searchIcon}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            className={styles.searchInput}
            type="text"
            placeholder="Search models..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-label="Search models"
          />
        </div>
        <div className={styles.filters}>
          {(['all', 'enabled', 'disabled'] as const).map((f) => (
            <button
              key={f}
              className={`${styles.chip} ${filter === f ? styles.chipActive : ''}`}
              onClick={() => setFilter(f)}
            >
              {f === 'all' ? 'All' : f === 'enabled' ? 'Enabled' : 'Disabled'}
            </button>
          ))}
        </div>
      </div>

      {/* Summary */}
      {!loading && models.length > 0 && (
        <div className={styles.summary}>
          <span className={styles.summaryText}>
            {totalEnabled} of {models.length} models enabled
          </span>
          {search && (
            <span className={styles.summaryText}>
              — {filtered.length} matching "{search}"
            </span>
          )}
        </div>
      )}

      {/* Content */}
      {loading ? (
        <>
          <LoadingSkeleton />
          <LoadingSkeleton />
        </>
      ) : models.length === 0 ? (
        <div className={styles.providerSection}>
          <div className={styles.providerCard}>
            <Table>
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Status</th>
                  <th className={styles.statusCol}></th>
                </tr>
              </thead>
              <tbody>
                <TableEmpty colSpan={4}>
                  <div className={styles.empty}>
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <rect x="2" y="3" width="20" height="14" rx="2" ry="2" />
                      <line x1="8" y1="21" x2="16" y2="21" />
                      <line x1="12" y1="17" x2="12" y2="21" />
                    </svg>
                    <span className={styles.emptyTitle}>No models discovered</span>
                    <span className={styles.emptyHint}>Models will appear here once the gateway discovers them from your infrastructure</span>
                  </div>
                </TableEmpty>
              </tbody>
            </Table>
          </div>
        </div>
      ) : filtered.length === 0 ? (
        <div className={styles.providerSection}>
          <div className={styles.providerCard}>
            <Table>
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Status</th>
                  <th className={styles.statusCol}></th>
                </tr>
              </thead>
              <tbody>
                <TableEmpty colSpan={4}>
                  <div className={styles.empty}>
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <circle cx="11" cy="11" r="8" />
                      <line x1="21" y1="21" x2="16.65" y2="16.65" />
                    </svg>
                    <span className={styles.emptyTitle}>No models match your filters</span>
                    <span className={styles.emptyHint}>Try adjusting your search or filter criteria</span>
                  </div>
                </TableEmpty>
              </tbody>
            </Table>
          </div>
        </div>
      ) : (
        providerGroups.map(([provider, providerModels]) => (
          <ProviderSection
            key={provider}
            provider={provider}
            models={providerModels}
            togglingIds={togglingIds}
            onToggle={handleToggle}
          />
        ))
      )}
    </div>
  )
}
