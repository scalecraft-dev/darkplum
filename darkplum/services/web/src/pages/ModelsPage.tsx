import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import {
  ApiError,
  deleteModelAlias,
  getRoutingPolicy,
  listAdminModels,
  listModelAliases,
  listPolicyPrincipals,
  putModelAlias,
  setModelDisplayName,
  toggleModel,
  updateRoutingPolicy,
} from '../lib/api'
import { formatModelName } from '../lib/formatModelName'
import { UNREACHABLE_HINT, backendClassLabel, backendClassOf, isReachable } from '../lib/modelRouting'
import { orgPolicyRestricted } from '../lib/policy'
import type { AdminModel, ModelAlias, ModelAliasInput, PolicyPrincipal, Residency, RoutingPolicy } from '../types'
import { useToast } from '../components/Toast'
import { Badge, Tab, TabList, TabPanel, Table, TableEmpty } from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import { Skeleton } from '../components/ui/Skeleton'
import { AccessOverviewSection } from './AccessOverviewSection'
import { AliasesSection } from './AliasesSection'
import { RoutingPolicyCard, type OrgPolicyDraft } from './RoutingPolicyCard'
import pageStyles from './PageLayout.module.css'
import styles from './ModelsPage.module.css'

/** Access overview page size. The API caps a page at 200; a smaller first
 *  page keeps the section quick to paint, and "Show more" fetches the rest. */
const PRINCIPALS_PAGE = 100

type Filter = 'all' | 'enabled' | 'disabled'

/** The three things this page is: a catalogue, the aliases that point into it,
 *  and the policy that decides which of it anyone actually reaches. Each is a
 *  tab; `?tab=` names the open one so any of them can be linked to. */
type TabKey = 'models' | 'aliases' | 'policy'
/** Prefix for this page's tab/panel DOM ids, so another tab set on the same
 *  page could never share an id with these. */
const TAB_SCOPE = 'models-page'

function tabFromParam(value: string | null): TabKey {
  return value === 'aliases' || value === 'policy' ? value : 'models'
}

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
// Badge glyphs — the state is never carried by colour alone.
// ---------------------------------------------------------------------------

function LockGlyph() {
  return (
    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className={styles.badgeIcon}>
      <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </svg>
  )
}

function UnreachableGlyph() {
  return (
    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className={styles.badgeIcon}>
      <circle cx="12" cy="12" r="10" />
      <line x1="4.93" y1="4.93" x2="19.07" y2="19.07" />
    </svg>
  )
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
// Display name
//
// The name everyone else picks this model by. A discovered id formatted for
// reading is the default; an administrator can replace it, and clearing the
// field goes back to the default rather than leaving a model with no name.
// ---------------------------------------------------------------------------

function DisplayNameCell({
  model,
  onRename,
}: {
  model: AdminModel
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
}) {
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState(model.display_name ?? '')
  const [saving, setSaving] = useState(false)
  const shown = model.display_name ?? formatModelName(model.model_id)

  const commit = async () => {
    const next = value.trim()
    setEditing(false)
    if (next === (model.display_name ?? '')) return
    setSaving(true)
    try {
      await onRename(model, next === '' ? null : next.slice(0, 100))
    } finally {
      setSaving(false)
    }
  }

  if (editing) {
    return (
      <input
        className={styles.nameInput}
        value={value}
        autoFocus
        maxLength={100}
        aria-label={`Display name for ${model.model_id}`}
        placeholder={formatModelName(model.model_id)}
        onChange={(e) => setValue(e.target.value)}
        onBlur={() => void commit()}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            void commit()
          } else if (e.key === 'Escape') {
            e.preventDefault()
            setValue(model.display_name ?? '')
            setEditing(false)
          }
        }}
      />
    )
  }

  return (
    <button
      type="button"
      className={styles.nameBtn}
      disabled={saving}
      onClick={() => {
        setValue(model.display_name ?? '')
        setEditing(true)
      }}
      title={model.display_name ? 'Rename — clear the field to use the discovered name' : 'Give this model a name'}
      aria-label={`Rename ${shown}`}
    >
      <span className={styles.modelName}>{saving ? 'Saving…' : shown}</span>
      <svg className={styles.nameIcon} width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M11.2 2.3a1.5 1.5 0 0 1 2.1 2.1L5.6 12.1l-2.8.7.7-2.8z" />
      </svg>
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
  residency: Residency
  /** The organisation's allowlist, or null when every enabled model is allowed. */
  allowedModels: string[] | null
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
}

function ProviderSection({ provider, models, togglingIds, residency, allowedModels, onToggle, onRename }: ProviderSectionProps) {
  const [collapsed, setCollapsed] = useState(false)

  const enabledCount = models.filter((m) => m.enabled).length
  const unreachableCount = models.filter((m) => !isReachable(m)).length

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
    const cls = backendClassOf(m)
    // Enabled, but the organisation's policy refuses it anyway: residency
    // refuses cloud-managed models; an allowlist refuses anything not on it.
    const residencyBlocked = m.enabled && residency === 'self_hosted_only' && cls === 'cloud_managed'
    const allowlistBlocked = m.enabled && allowedModels !== null && !allowedModels.includes(m.model_id)
    const blocked = residencyBlocked || allowlistBlocked
    // Configured, but the gateway has not seen its backend for a few minutes.
    const unreachable = !isReachable(m)
    // One hint per row. A policy refusal outranks an unreachable backend: it
    // still refuses the request once the backend comes back.
    const hint = residencyBlocked
      ? 'Enabled, but refused while routing is self-hosted only.'
      : allowlistBlocked
        ? "Enabled, but not on the organisation's allowed list, so refused."
        : unreachable
          ? UNREACHABLE_HINT
          : null
    return (
      <tr key={m.id} className={blocked || unreachable ? styles.rowFlagged : ''}>
        <td>
          <div className={styles.modelCell}>
            <DisplayNameCell model={m} onRename={onRename} />
          </div>
        </td>
        <td>
          <div className={styles.modelIdCell}>
            <code className={styles.modelId}>{m.model_id}</code>
            <CopyButton value={m.model_id} label="Copy" />
          </div>
        </td>
        <td>
          <Badge variant={cls === 'cloud_managed' ? 'info' : 'neutral'} bordered>
            {backendClassLabel(cls)}
          </Badge>
        </td>
        <td>
          <div className={styles.statusCell}>
            <div className={styles.statusBadges}>
              {/* Configuration first: it is what the toggle beside it controls. */}
              <Badge variant={m.enabled ? 'success' : 'neutral'}>
                {m.enabled ? 'Enabled' : 'Disabled'}
              </Badge>
              {blocked && (
                <Badge variant="warning" bordered>
                  <LockGlyph />
                  Blocked by policy
                </Badge>
              )}
              {unreachable && (
                <Badge variant="warning" bordered>
                  <UnreachableGlyph />
                  Not reachable
                </Badge>
              )}
            </div>
            {hint && <span className={styles.statusHint}>{hint}</span>}
          </div>
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
            {/* Counted apart from "enabled": an unreachable model can be either. */}
            {unreachableCount > 0 && (
              <span className={styles.providerUnreachable}>
                {unreachableCount} not reachable
              </span>
            )}
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
                <Table label={`${vendorLabel(vendor)} models on ${providerLabel(provider)}`}>
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th>Model ID</th>
                      <th>Hosting</th>
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
            <Table label={`${providerLabel(provider)} models`}>
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Hosting</th>
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
              <Skeleton width="220px" height="14px" />
              <Skeleton width="80px" height="18px" borderRadius="var(--radius-full)" />
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
  // The open tab is read from the URL rather than held beside it, so a deep
  // link (?tab=policy) and the page's own cross-tab links are the same thing.
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = tabFromParam(searchParams.get('tab'))
  const selectTab = useCallback(
    (next: TabKey) => {
      const params = new URLSearchParams(searchParams)
      params.set('tab', next)
      // Replace: switching tabs is not a place someone navigated to, so Back
      // should leave the page rather than walk back through the tabs.
      setSearchParams(params, { replace: true })
    },
    [searchParams, setSearchParams],
  )

  const [models, setModels] = useState<AdminModel[]>([])
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [togglingIds, setTogglingIds] = useState<Set<string>>(new Set())

  const [policy, setPolicy] = useState<RoutingPolicy | null>(null)
  const [policyLoading, setPolicyLoading] = useState(true)
  const [policyError, setPolicyError] = useState<string | null>(null)
  const [policySaving, setPolicySaving] = useState(false)

  const [aliases, setAliases] = useState<ModelAlias[]>([])
  const [aliasesLoading, setAliasesLoading] = useState(true)
  const [aliasesError, setAliasesError] = useState<string | null>(null)

  const [principals, setPrincipals] = useState<PolicyPrincipal[]>([])
  const [principalsTotal, setPrincipalsTotal] = useState(0)
  const [principalsLoading, setPrincipalsLoading] = useState(true)
  const [principalsLoadingMore, setPrincipalsLoadingMore] = useState(false)
  const [principalsError, setPrincipalsError] = useState<string | null>(null)

  const residency: Residency = policy?.residency ?? 'any'
  const allowedModels: string[] | null = policy?.allowed_models ?? null

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

  // Policy and aliases report their own failures inside their card rather than
  // as a toast: the reason belongs next to the control it disables.
  const loadPolicy = useCallback(async () => {
    setPolicyLoading(true)
    try {
      setPolicy(await getRoutingPolicy())
      setPolicyError(null)
    } catch (err) {
      setPolicyError((err as Error).message)
    } finally {
      setPolicyLoading(false)
    }
  }, [])

  /** Reloads the first page. Anything that can change a principal's reachable
   *  set (the org policy, a per-principal edit) calls this, so the list is
   *  never a mix of before and after. */
  const loadPrincipals = useCallback(async () => {
    setPrincipalsLoading(true)
    try {
      const page = await listPolicyPrincipals({ limit: PRINCIPALS_PAGE, offset: 0 })
      setPrincipals(page.principals)
      setPrincipalsTotal(page.total)
      setPrincipalsError(null)
    } catch (err) {
      setPrincipalsError((err as Error).message)
    } finally {
      setPrincipalsLoading(false)
    }
  }, [])

  /** Appends the next page. The API paginates by offset over a stable order,
   *  so the offset is simply how many rows are already shown. */
  const loadMorePrincipals = useCallback(async () => {
    setPrincipalsLoadingMore(true)
    try {
      const page = await listPolicyPrincipals({ limit: PRINCIPALS_PAGE, offset: principals.length })
      setPrincipals((prev) => {
        const seen = new Set(prev.map((p) => `${p.principal_type}:${p.user_id ?? p.api_key_id}`))
        return [...prev, ...page.principals.filter((p) => !seen.has(`${p.principal_type}:${p.user_id ?? p.api_key_id}`))]
      })
      setPrincipalsTotal(page.total)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setPrincipalsLoadingMore(false)
    }
  }, [principals.length, toast])

  const loadAliases = useCallback(async () => {
    setAliasesLoading(true)
    try {
      setAliases(await listModelAliases())
      setAliasesError(null)
    } catch (err) {
      setAliasesError((err as Error).message)
    } finally {
      setAliasesLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadModels()
    void loadPolicy()
    void loadAliases()
    void loadPrincipals()
  }, [loadModels, loadPolicy, loadAliases, loadPrincipals])

  /** Resolves with what the API said the change does: the routed-alias routes
   *  that will now be refused, how many principals with a policy of their own
   *  reach less than before, and whether everyone who inherits reaches less
   *  too. All three are the API's own counts — nothing here is inferred. A
   *  refusal (409: the policy would leave someone with no model) comes back
   *  as `lockedOut`, the API's list of who, so the card can show it in place. */
  const handlePolicyChange = async (
    draft: OrgPolicyDraft,
  ): Promise<{
    warnings: string[]
    affectedPrincipals: number
    orgNarrowed: boolean
    unresolvedModels: string[]
    lockedOut: string[]
  }> => {
    setPolicySaving(true)
    try {
      const updated = await updateRoutingPolicy({
        residency: draft.residency,
        allowed_models: draft.allowed_models,
        default_model: draft.default_model,
      })
      setPolicy(updated)
      toast('Routing policy saved', 'success')
      // The aliases carry per-route health, and the principals carry reachable
      // sets; this may have just changed both.
      void loadAliases()
      void loadPrincipals()
      return {
        warnings: updated.warnings ?? [],
        affectedPrincipals: updated.affected_principals ?? 0,
        orgNarrowed: updated.org_narrowed ?? false,
        unresolvedModels: updated.unresolved_models ?? [],
        lockedOut: [],
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        const lockedOut = err.lockedOut()
        return {
          warnings: [],
          affectedPrincipals: 0,
          orgNarrowed: false,
          unresolvedModels: [],
          lockedOut: lockedOut.length > 0 ? lockedOut : ['(organisation)'],
        }
      }
      toast((err as Error).message, 'error')
      return { warnings: [], affectedPrincipals: 0, orgNarrowed: false, unresolvedModels: [], lockedOut: [] }
    } finally {
      setPolicySaving(false)
    }
  }

  /** Renames a model for everyone who picks one. The API is the authority on
   *  what was stored, so the row takes the response rather than the input. */
  const handleRename = async (model: AdminModel, displayName: string | null) => {
    try {
      const updated = await setModelDisplayName(model.id, displayName)
      setModels((prev) => prev.map((m) => (m.id === updated.id ? updated : m)))
      toast(
        displayName === null
          ? `Name cleared — ${model.model_id} uses its discovered name again`
          : `Renamed to ${displayName}`,
        'success',
      )
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }

  // Throws on failure so the editor can attach the server's message to a field.
  // Resolves with the API's configure-time warnings for the saved alias.
  const handleSaveAlias = async (alias: string, body: ModelAliasInput): Promise<string[]> => {
    const existing = aliases.some((a) => a.alias === alias)
    const saved = await putModelAlias(alias, body)
    setAliases((prev) => {
      const next = prev.filter((a) => a.alias !== saved.alias)
      return [...next, saved].sort((a, b) => a.alias.localeCompare(b.alias))
    })
    const changed =
      'kind' in body
        ? `now has ${body.routes.length} ${body.routes.length === 1 ? 'route' : 'routes'}`
        : `now points at ${body.target_model_id}`
    toast(existing ? `Alias ${alias} ${changed}` : `Alias ${alias} created`, 'success')
    return saved.warnings ?? []
  }

  const handleDeleteAlias = async (alias: string) => {
    await deleteModelAlias(alias)
    setAliases((prev) => prev.filter((a) => a.alias !== alias))
  }

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
  const totalUnreachable = models.filter((m) => !isReachable(m)).length

  /** The same test the policy card uses to mark itself restricted. On the
   *  catalogue tab it is the reason a row can be enabled and still refused, so
   *  it is said there too rather than only on the tab that owns it. */
  const policyRestricted = orgPolicyRestricted(policy)

  /** Switching tabs from a link INSIDE a panel unmounts that panel, and the
   *  focused link with it — the browser then drops focus to <body>. The tab
   *  row stays mounted, so focus is handed to the tab that was just opened. */
  const goToTab = useCallback(
    (next: TabKey) => {
      selectTab(next)
      requestAnimationFrame(() => document.getElementById(`tab-${TAB_SCOPE}-${next}`)?.focus())
    },
    [selectTab],
  )

  return (
    <div className={pageStyles.page}>
      <div className={pageStyles.header}>
        <h1 className={pageStyles.title}>Models</h1>
        <p className={pageStyles.subtitle}>Choose what your organization can send requests to</p>
      </div>

      <TabList label="Models sections" scope={TAB_SCOPE}>
        <Tab
          id="models"
          active={tab === 'models'}
          count={totalEnabled > 0 ? totalEnabled : undefined}
          onClick={() => selectTab('models')}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <rect x="3" y="3" width="7" height="7" />
            <rect x="14" y="3" width="7" height="7" />
            <rect x="14" y="14" width="7" height="7" />
            <rect x="3" y="14" width="7" height="7" />
          </svg>
          Models
        </Tab>
        <Tab
          id="aliases"
          active={tab === 'aliases'}
          count={aliases.length > 0 ? aliases.length : undefined}
          onClick={() => selectTab('aliases')}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z" />
            <line x1="7" y1="7" x2="7.01" y2="7" />
          </svg>
          Aliases
        </Tab>
        <Tab
          id="policy"
          active={tab === 'policy'}
          count={principalsTotal > 0 ? principalsTotal : undefined}
          onClick={() => selectTab('policy')}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
          </svg>
          Policy
        </Tab>
      </TabList>

      <TabPanel id="models" scope={TAB_SCOPE} active={tab === 'models'}>
        <ModelsTab
          models={models}
          filtered={filtered}
          providerGroups={providerGroups}
          loading={loading}
          search={search}
          filter={filter}
          togglingIds={togglingIds}
          residency={residency}
          allowedModels={allowedModels}
          totalEnabled={totalEnabled}
          totalUnreachable={totalUnreachable}
          policyRestricted={policyRestricted}
          onGoToTab={goToTab}
          onSearch={setSearch}
          onFilter={setFilter}
          onToggle={handleToggle}
          onRename={handleRename}
        />
      </TabPanel>

      <TabPanel id="aliases" scope={TAB_SCOPE} active={tab === 'aliases'}>
        <AliasesSection
          aliases={aliases}
          models={models}
          loading={aliasesLoading || loading}
          error={aliasesError}
          residency={residency}
          onRetry={() => void loadAliases()}
          onSave={handleSaveAlias}
          onDelete={handleDeleteAlias}
        />
      </TabPanel>

      <TabPanel id="policy" scope={TAB_SCOPE} active={tab === 'policy'}>
        <RoutingPolicyCard
          policy={policy}
          loading={policyLoading}
          error={policyError}
          saving={policySaving}
          models={models}
          aliases={aliases}
          onRetry={() => void loadPolicy()}
          onChange={handlePolicyChange}
        />

        <AccessOverviewSection
          principals={principals}
          total={principalsTotal}
          models={models}
          loading={principalsLoading}
          loadingMore={principalsLoadingMore}
          error={principalsError}
          onRetry={() => void loadPrincipals()}
          onLoadMore={() => void loadMorePrincipals()}
        />
      </TabPanel>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Models tab — the catalogue
// ---------------------------------------------------------------------------

interface ModelsTabProps {
  models: AdminModel[]
  filtered: AdminModel[]
  providerGroups: [string, AdminModel[]][]
  loading: boolean
  search: string
  filter: Filter
  togglingIds: Set<string>
  residency: Residency
  allowedModels: string[] | null
  totalEnabled: number
  totalUnreachable: number
  policyRestricted: boolean
  onGoToTab: (tab: TabKey) => void
  onSearch: (value: string) => void
  onFilter: (value: Filter) => void
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
}

function ModelsTab({
  models,
  filtered,
  providerGroups,
  loading,
  search,
  filter,
  togglingIds,
  residency,
  allowedModels,
  totalEnabled,
  totalUnreachable,
  policyRestricted,
  onGoToTab,
  onSearch,
  onFilter,
  onToggle,
  onRename,
}: ModelsTabProps) {
  return (
    <>
      {/* The catalogue on its own reads as "everything here is reachable". When
          the policy says otherwise, that is said once, here, with the way to
          go and read it. */}
      {policyRestricted && (
        <p className={styles.policyNote}>
          <LockGlyph />
          <span>
            The organisation's policy limits what is reachable —{' '}
            <Link
              className={styles.policyNoteLink}
              to={{ search: '?tab=policy' }}
              onClick={(e) => {
                // A plain click switches in place and hands focus to the tab;
                // a modified or middle click keeps the browser's own
                // open-in-new-tab behaviour via the real href.
                if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
                e.preventDefault()
                onGoToTab('policy')
              }}
            >
              Policy tab
            </Link>
          </span>
        </p>
      )}

      {/* Named for anyone navigating by heading, like the other two tabs; the
          visible name is the tab itself. */}
      <h2 className={styles.srOnly}>Available models</h2>

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
            onChange={(e) => onSearch(e.target.value)}
            aria-label="Search models"
          />
        </div>
        <div className={styles.filters}>
          {(['all', 'enabled', 'disabled'] as const).map((f) => (
            <button
              key={f}
              className={`${styles.chip} ${filter === f ? styles.chipActive : ''}`}
              onClick={() => onFilter(f)}
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
          {totalUnreachable > 0 && (
            <span className={styles.summaryFlag}>
              <UnreachableGlyph />
              {totalUnreachable} not reachable right now
            </span>
          )}
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
            <Table label="Models">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Hosting</th>
                  <th>Status</th>
                  <th className={styles.statusCol}></th>
                </tr>
              </thead>
              <tbody>
                <TableEmpty colSpan={5}>
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
            <Table label="Models matching your filters">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Model ID</th>
                  <th>Hosting</th>
                  <th>Status</th>
                  <th className={styles.statusCol}></th>
                </tr>
              </thead>
              <tbody>
                <TableEmpty colSpan={5}>
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
            residency={residency}
            allowedModels={allowedModels}
            onToggle={onToggle}
            onRename={onRename}
          />
        ))
      )}
    </>
  )
}
