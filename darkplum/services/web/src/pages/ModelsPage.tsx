import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, Navigate, useSearchParams } from 'react-router-dom'
import {
  ApiError,
  getRoutingPolicy,
  listAdminModels,
  listModelAliases,
  listPolicyPrincipals,
  setModelDisplayName,
  setVendorFilter,
  toggleModel,
  updateRoutingPolicy,
} from '../lib/api'
import { formatModelName } from '../lib/formatModelName'
import {
  UNKNOWN_CLASS_HINT,
  UNREACHABLE_HINT,
  UNSUPPORTED_CONTENT_NOTE,
  aliasReferenceConsequence,
  aliasReferencesTo,
  backendClassLabel,
  backendClassTooltip,
  backendClassVariant,
  compareProviders,
  displayClassOf,
  isEgress,
  isReachable,
  isRoutedAlias,
  isVendorProvider,
  providerLabel,
  vendorLabel,
} from '../lib/modelRouting'
import {
  DEFAULT_RESIDENCY,
  orgPolicyRestricted,
  residencyLabel,
  residencyPermits,
} from '../lib/policy'
import { canInstallKeyOverTransport, installPhase, keyTransportHelp } from '../lib/vendorKeys'
import type { KeyTransportHelp } from '../lib/vendorKeys'
import type { AliasReference } from '../lib/modelRouting'
import type {
  AdminModel,
  DisplayClass,
  ModelAlias,
  PolicyPrincipal,
  ProviderStatus,
  Residency,
  RoutingPolicy,
  VendorName,
} from '../types'
import { useToast } from '../components/Toast'
import {
  Badge,
  Button,
  ClassGlyph,
  EgressGlyph,
  LockGlyph,
  Modal,
  OptInGlyph,
  Tab,
  TabList,
  TabPanel,
  Table,
  TableEmpty,
  UnknownGlyph,
  UnreachableGlyph,
} from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import {
  AWS_OPT_IN_LABEL,
  awsOptInCountText,
  awsOptInStatusHint,
  needsAwsOptIn,
} from '../lib/dataRetention'
import { Skeleton } from '../components/ui/Skeleton'
import { AccessOverviewSection } from './AccessOverviewSection'
import { RoutingPolicyCard, type OrgPolicyDraft } from './RoutingPolicyCard'
import { VendorEnableModal } from './VendorEnableModal'
import { VendorInstallModal, type Outcome, type VendorInstallMode } from './VendorInstallModal'
import { VendorStatusSection } from './VendorStatusSection'
import { VendorBillingKeyModal, type BillingKeyMode } from './VendorBillingKeyModal'
import pageStyles from './PageLayout.module.css'
import { PricesTab } from './PricesTab'
import styles from './ModelsPage.module.css'

/** Access overview page size. The API caps a page at 200; a smaller first
 *  page keeps the section quick to paint, and "Show more" fetches the rest. */
const PRINCIPALS_PAGE = 100

/** How often the catalogue is re-read while a vendor key or filter is landing.
 *  The api syncs on the same cadence over that window, so this is not a poll
 *  against a value that cannot have changed. */
const INSTALLING_POLL_MS = 5_000

/** A vendor with no stored filter, as one array rather than a fresh literal:
 *  the key modal seeds its checklist from this and must not re-seed because a
 *  poll handed it a new empty array. */
const EMPTY_FILTER: string[] = []

type Filter = 'all' | 'enabled' | 'disabled'

/** The two things this page is: a catalogue, and the policy that decides
 *  which of it anyone actually reaches. Each is a tab; `?tab=` names the open
 *  one so either can be linked to. Aliases and routed aliases have their own
 *  page, Admin > Routing; `?tab=aliases` is redirected there so old links land. */
type TabKey = 'models' | 'policy' | 'prices'
/** Prefix for this page's tab/panel DOM ids, so another tab set on the same
 *  page could never share an id with these. */
const TAB_SCOPE = 'models-page'

function tabFromParam(value: string | null): TabKey {
  return value === 'policy' || value === 'prices' ? value : 'models'
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

/** A Bedrock sub-group heading. Unlike a vendor provider, this is the company
 *  that made the weights, not the company being called. */
function bedrockVendorHeading(vendor: string): string {
  return vendor ? vendorLabel(vendor) : 'Other'
}

// ---------------------------------------------------------------------------
// Why a row is refused
//
// One function, so the badge, the hint and the counts on this page can never
// disagree about a rule the gateway enforces once. The order matters: a model
// that is off is not "blocked by policy", it is off, and telling an
// administrator to change the policy would be sending them to the wrong screen.
// ---------------------------------------------------------------------------

type RowFlag =
  | { kind: 'disabled_by_policy'; hint: string }
  | { kind: 'blocked'; hint: string }
  | { kind: 'unknown'; hint: string }
  | { kind: 'aws_opt_in'; hint: string }
  | { kind: 'unreachable'; hint: string }
  | null

function rowFlag(
  m: AdminModel,
  residency: Residency,
  allowedModels: string[] | null,
): RowFlag {
  const cls = displayClassOf(m)
  const permitted = residencyPermits(residency, cls)
  const onAllowlist = allowedModels === null || allowedModels.includes(m.model_id)

  // A disabled row is not being refused by anything — it is off. It still
  // earns a note when turning it on would not be enough, because "enable it
  // and it works" is the reasonable assumption and it would be wrong.
  if (!m.enabled) {
    if (cls === 'unknown') return { kind: 'unknown', hint: UNKNOWN_CLASS_HINT }
    if (!permitted) {
      return {
        kind: 'disabled_by_policy',
        hint: `Turning this on is not enough: "${residencyLabel(residency)}" refuses ${backendClassLabel(cls).toLowerCase()} models.`,
      }
    }
    if (!onAllowlist) {
      return {
        kind: 'disabled_by_policy',
        hint: "Turning this on is not enough: it is not on the organisation's allowed list.",
      }
    }
    // Off, and AWS would refuse it once on — the same "turning this on is not
    // enough" situation, explained in the account's own terms.
    if (needsAwsOptIn(m)) return { kind: 'aws_opt_in', hint: awsOptInStatusHint(m) }
    return null
  }

  // Enabled, and refused anyway. The unknown class is its own answer: the fix
  // is a backend reporting it, not a policy change.
  if (cls === 'unknown') return { kind: 'unknown', hint: UNKNOWN_CLASS_HINT }
  if (!permitted) {
    return {
      kind: 'blocked',
      hint: `Enabled, but refused: "${residencyLabel(residency)}" does not permit ${backendClassLabel(cls).toLowerCase()} models.`,
    }
  }
  if (!onAllowlist) {
    return {
      kind: 'blocked',
      hint: "Enabled, but not on the organisation's allowed list, so refused.",
    }
  }
  // Precedence: Blocked by policy > Needs AWS opt-in > Not reachable. A policy
  // refusal still refuses once the account is changed, and an account that
  // refuses the model still refuses once the backend comes back. 'unknown'
  // availability says nothing and falls through.
  if (needsAwsOptIn(m)) return { kind: 'aws_opt_in', hint: awsOptInStatusHint(m) }
  if (!isReachable(m)) return { kind: 'unreachable', hint: UNREACHABLE_HINT }
  return null
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
  /** True for a vendor whose key works and whose models are all still off:
   *  the one moment the first-run banner belongs on this card. */
  firstRun: boolean
  onReviewAndEnable: (vendor: VendorName) => void
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
}

function ProviderSection({
  provider,
  models,
  togglingIds,
  residency,
  allowedModels,
  firstRun,
  onReviewAndEnable,
  onToggle,
  onRename,
}: ProviderSectionProps) {
  const [collapsed, setCollapsed] = useState(false)

  const enabledCount = models.filter((m) => m.enabled).length
  const unreachableCount = models.filter((m) => !isReachable(m)).length

  /** The class of this provider's models. Providers are homogeneous in
   *  practice, so one badge on the header says it for every row beneath —
   *  and where they are not, the per-row badge still tells the truth. */
  const headerClass: DisplayClass = useMemo(() => {
    const classes = new Set(models.map((m) => displayClassOf(m)))
    return classes.size === 1 ? [...classes][0] : 'unknown'
  }, [models])

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
    const cls = displayClassOf(m)
    const flag = rowFlag(m, residency, allowedModels)
    // Configured, but the gateway has not seen its backend for a few minutes.
    // Counted apart from the flag, which reports only the top problem.
    const unreachable = !isReachable(m)
    return (
      <tr key={m.id} className={flag ? styles.rowFlagged : ''}>
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
          <Badge
            variant={backendClassVariant(cls)}
            bordered
            dashed={cls === 'unknown'}
            title={backendClassTooltip(cls)}
          >
            <ClassGlyph cls={cls} />
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
              {flag?.kind === 'blocked' && (
                <Badge variant="warning" bordered>
                  <LockGlyph />
                  Blocked by policy
                </Badge>
              )}
              {/* A row that is off and would still be refused if turned on.
                  Named differently from a live refusal, because nothing is
                  being refused yet — this is about what would happen. */}
              {flag?.kind === 'disabled_by_policy' && (
                <Badge variant="neutral" bordered>
                  <LockGlyph />
                  Blocked by policy
                </Badge>
              )}
              {/* Warning while it is on and being refused; neutral while it
                  is off, like the disabled policy badge — nothing is being
                  refused yet. */}
              {flag?.kind === 'aws_opt_in' && (
                <Badge variant={m.enabled ? 'warning' : 'neutral'} bordered>
                  <OptInGlyph />
                  {AWS_OPT_IN_LABEL}
                </Badge>
              )}
              {flag?.kind === 'unknown' && (
                <Badge variant="neutral" bordered dashed>
                  <UnknownGlyph />
                  Not reported
                </Badge>
              )}
              {unreachable && (
                <Badge variant="warning" bordered>
                  <UnreachableGlyph />
                  Not reachable
                </Badge>
              )}
            </div>
            {flag && <span className={styles.statusHint}>{flag.hint}</span>}
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
            {/* Where everything under this header runs. On the header rather
                than only per row, because the decision an administrator makes
                here — expand this card at all? — is about the whole group. */}
            <Badge
              variant={backendClassVariant(headerClass)}
              bordered
              dashed={headerClass === 'unknown'}
              title={backendClassTooltip(headerClass)}
            >
              <ClassGlyph cls={headerClass} />
              {backendClassLabel(headerClass)}
            </Badge>
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
          {/* The one moment this banner is worth showing: the key works, the
              models are here, and not one of them is on. It disappears the
              instant any of them is — including when it is turned on from the
              row switch rather than from the button. */}
          {firstRun && (
            <div className={styles.firstRun}>
              <div className={styles.firstRunText}>
                <p className={styles.firstRunTitle}>
                  {providerLabel(provider)} is connected, and none of its models are on yet.
                </p>
                <p className={styles.firstRunBody}>
                  Turning these on sends prompts to {providerLabel(provider)}, outside your cloud
                  account. Review what that means and pick which models before anything changes.
                </p>
              </div>
              <Button
                size="sm"
                variant="secondary"
                onClick={() => onReviewAndEnable(provider as VendorName)}
              >
                Review and enable
              </Button>
            </div>
          )}

          {/* The note about what a request may carry used to hang here, on each
              vendor card. It is not a vendor limit any more — tool calls and
              images are refused on every backend — so repeating it per vendor
              said the opposite of what is true. It is stated once, above the
              catalogue. */}

          {vendorGroups ? (
            // Bedrock: render sub-groups with vendor headings
            vendorGroups.map(([vendor, vendorModels]) => (
              <div key={vendor}>
                <div className={styles.vendorHeading}>{bedrockVendorHeading(vendor)}</div>
                <Table label={`${bedrockVendorHeading(vendor)} models on ${providerLabel(provider)}`}>
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

/** Before Routing had its own page, aliases were a tab here. A bookmark or a
 *  link written then (`/admin/models?tab=aliases`) goes to where they live now,
 *  replacing the history entry so Back does not bounce through the redirect. */
export function ModelsPage() {
  const [searchParams] = useSearchParams()
  if (searchParams.get('tab') === 'aliases') return <Navigate to="/admin/routing" replace />
  return <ModelsPageBody />
}

function ModelsPageBody() {
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
  /** Vendor status, from the same response as the catalogue: the gateway
   *  reports both in one sync, so reading them apart could show a vendor as
   *  connected next to a catalogue that predates the key. */
  const [providers, setProviders] = useState<ProviderStatus[]>([])
  /** What the gateway behind this api can do. Decides whether the vendor card
   *  offers a form or the command line — never a form that fails on submit. */
  const [providerFeatures, setProviderFeatures] = useState<string[]>([])
  /** Whether the api would accept a key over this page's transport. Null until
   *  it has answered, and on an api that predates the field — read through
   *  canInstallKeyOverTransport, which falls back to the browser's own view. */
  const [keyTransportOk, setKeyTransportOk] = useState<boolean | null>(null)
  /** The last removal's warnings, by vendor. Held on the page rather than in
   *  the modal because the modal closes on success and the card is where the
   *  removal is watched; cleared when that vendor gets a key again. */
  const [removalWarnings, setRemovalWarnings] = useState<Partial<Record<VendorName, string[]>>>({})
  /** Which vendor's key modal is open, and which errand it is on. One at a
   *  time: they all write the same two objects. */
  const [managing, setManaging] = useState<{ vendor: VendorName; mode: VendorInstallMode } | null>(
    null,
  )
  /** The billing-key modal: a separate credential, a separate modal. */
  const [managingBilling, setManagingBilling] = useState<
    { vendor: VendorName; mode: BillingKeyMode } | null
  >(null)
  /** Which vendor's "Review and enable" is open, or null. One at a time: it
   *  writes the organisation's policy, and two of those racing is not a state
   *  worth supporting. */
  const [enablingVendor, setEnablingVendor] = useState<VendorName | null>(null)
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [togglingIds, setTogglingIds] = useState<Set<string>>(new Set())

  const [policy, setPolicy] = useState<RoutingPolicy | null>(null)
  const [policyLoading, setPolicyLoading] = useState(true)
  const [policyError, setPolicyError] = useState<string | null>(null)
  const [policySaving, setPolicySaving] = useState(false)

  /** Read for two things only: the disable check (does an alias point at this
   *  model?) and the policy card's "add every target of alias" helper. They are
   *  edited on Admin > Routing; this page never writes them. */
  const [aliases, setAliases] = useState<ModelAlias[]>([])
  /** False until a read has succeeded. The disable check reads afresh rather
   *  than trust an empty list that is only empty because the read failed. */
  const [aliasesLoaded, setAliasesLoaded] = useState(false)
  /** A disable waiting on confirmation because aliases point at the model. */
  const [pendingDisable, setPendingDisable] = useState<
    { model: AdminModel; refs: AliasReference[] } | null
  >(null)

  const [principals, setPrincipals] = useState<PolicyPrincipal[]>([])
  const [principalsTotal, setPrincipalsTotal] = useState(0)
  const [principalsLoading, setPrincipalsLoading] = useState(true)
  const [principalsLoadingMore, setPrincipalsLoadingMore] = useState(false)
  const [principalsError, setPrincipalsError] = useState<string | null>(null)

  /**
   * The rung every "would this be refused?" question on this page is answered
   * against. Until the policy has loaded there is no answer, and the fallback
   * must not be the loosest rung: that would paint a vendor model as permitted
   * for the first frame after a reload, which is the one thing the class
   * labelling exists to prevent. The api's own default is the safe assumption,
   * and it never returns null for the organisation.
   */
  const residency: Residency = policy?.residency ?? DEFAULT_RESIDENCY
  const allowedModels: string[] | null = policy?.allowed_models ?? null

  const loadModels = useCallback(async () => {
    try {
      const result = await listAdminModels()
      setModels(result.models)
      setProviders(result.providers)
      setProviderFeatures(result.providerFeatures)
      setKeyTransportOk(result.keyInstallTransportOk)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }, [toast])

  // The policy reports its own failure inside its card rather than as a toast: the reason belongs next to the control it disables.
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

  /** Quiet on failure: nothing on this page displays the aliases, and the
   *  disable check re-reads them (and says so if that fails) before it acts. */
  const loadAliases = useCallback(async () => {
    try {
      setAliases(await listModelAliases())
      setAliasesLoaded(true)
    } catch {
      setAliasesLoaded(false)
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
    /** A 409 that was not a lockout — the API's own sentence, shown as it came. */
    refusal: string | null
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
      // The principals carry reachable sets; this may have just changed them.
      void loadPrincipals()
      return {
        warnings: updated.warnings ?? [],
        affectedPrincipals: updated.affected_principals ?? 0,
        orgNarrowed: updated.org_narrowed ?? false,
        unresolvedModels: updated.unresolved_models ?? [],
        lockedOut: [],
        refusal: null,
      }
    } catch (err) {
      // A 409 used to mean exactly one thing: this policy would leave someone
      // with no model. It can now also mean a routed alias has a route the new
      // rung refuses. The two are told apart by the STRUCTURED `locked_out`
      // field, never by the sentence — a 409 without it is reported in the
      // card as the API worded it, rather than being announced as a lockout
      // that did not happen.
      if (err instanceof ApiError && err.status === 409) {
        const lockedOut = err.lockedOut()
        return {
          warnings: [],
          affectedPrincipals: 0,
          orgNarrowed: false,
          unresolvedModels: [],
          lockedOut,
          refusal: lockedOut.length > 0 ? null : (err as Error).message,
        }
      }
      toast((err as Error).message, 'error')
      return {
        warnings: [],
        affectedPrincipals: 0,
        orgNarrowed: false,
        unresolvedModels: [],
        lockedOut: [],
        refusal: null,
      }
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

  /**
   * Turning a model off is checked against the aliases first: a static alias
   * on it, or the default route of a routed one, starts refusing, and any
   * other route moves to its alias's default. Nobody should learn that from a
   * chat transcript. A model nothing points at is turned off as before.
   */
  const handleToggle = async (model: AdminModel) => {
    if (!model.enabled) {
      await applyToggle(model)
      return
    }
    let current = aliases
    // A routed alias without its routes cannot be checked, and a list that
    // never loaded is not a list of nothing; read them fresh rather than guess
    // that nothing points here.
    if (!aliasesLoaded || current.some((a) => isRoutedAlias(a) && a.routes === undefined)) {
      setTogglingIds((prev) => new Set(prev).add(model.id))
      try {
        current = await listModelAliases()
        setAliases(current)
        setAliasesLoaded(true)
      } catch (err) {
        toast(`Could not check which aliases use this model: ${(err as Error).message}`, 'error')
        return
      } finally {
        setTogglingIds((prev) => {
          const next = new Set(prev)
          next.delete(model.id)
          return next
        })
      }
    }
    const refs = aliasReferencesTo(model.model_id, current)
    if (refs.length > 0) {
      setPendingDisable({ model, refs })
      return
    }
    await applyToggle(model)
  }

  const applyToggle = async (model: AdminModel) => {
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
    // Closest to the organisation first, furthest last — the same order the
    // residency ladder reads in, so the two screens agree about which end of
    // the range is which.
    return [...groups.entries()].sort(([a], [b]) => compareProviders(a, b))
  }, [filtered])

  const totalEnabled = models.filter((m) => m.enabled).length
  const totalUnreachable = models.filter((m) => !isReachable(m)).length
  const totalAwsOptIn = models.filter(needsAwsOptIn).length

  /**
   * Vendors whose key works and whose models are every one of them still off.
   *
   * Computed from the unfiltered catalogue, never from what the search box has
   * left on screen: a banner that appears because someone typed a word, or
   * disappears because they did, is not a banner anyone can trust.
   */
  const firstRunVendors = useMemo(() => {
    const set = new Set<VendorName>()
    for (const p of providers) {
      if (p.state !== 'ok') continue
      const theirs = models.filter((m) => m.provider === p.name)
      if (theirs.length > 0 && theirs.every((m) => !m.enabled)) set.add(p.name)
    }
    return set
  }, [providers, models])

  /** Vendors with at least one catalogued model that is not on yet — the only
   *  case where "Review and enable" has anything to do. Computed from the
   *  whole catalogue, never from what the search box has left on screen. */
  const enableableVendors = useMemo(() => {
    const set = new Set<VendorName>()
    for (const p of providers) {
      if (models.some((m) => m.provider === p.name && !m.enabled)) set.add(p.name)
    }
    return set
  }, [providers, models])

  /** The catalogue rows the key modal needs: what Change models starts from,
   *  and what a removal has to turn off first. */
  const managingModels = useMemo(
    () => (managing ? models.filter((m) => m.provider === managing.vendor) : []),
    [managing, models],
  )

  /** Everything the open modal works on, read from the live catalogue so a
   *  toggle made behind it is reflected when it is reopened. */
  const enablingModels = useMemo(
    () => (enablingVendor ? models.filter((m) => m.provider === enablingVendor) : []),
    [enablingVendor, models],
  )

  /**
   * While a key or a filter is on its way to the gateway, this page re-reads
   * itself every few seconds.
   *
   * The api ticks its own sync faster over the same window, so what is being
   * polled is genuinely moving. Nothing here is derived from a local timer:
   * the window comes from `installing_until` on the response, which is why
   * reloading mid-install picks the wait up rather than restarting it, and why
   * a second tab shows the same progress.
   */
  const inFlight = useMemo(
    () =>
      providers.some((p) => {
        const phase = installPhase(p, Date.now())
        // A removal is polled for the same reason as an install: the card
        // settles when the gateway reports the key gone, not on a timer.
        return phase === 'installing' || phase === 'removing'
      }),
    [providers],
  )
  useEffect(() => {
    if (!inFlight) return
    const id = setInterval(() => void loadModels(), INSTALLING_POLL_MS)
    return () => clearInterval(id)
  }, [inFlight, loadModels])

  /** Stores the filter the command line set, so this page becomes the writer
   *  of record for it. Rejects with the api's own message; the card shows it
   *  beside the button that was pressed. */
  const handleAdoptFilter = useCallback(
    async (vendor: VendorName, filter: string[]) => {
      await setVendorFilter(vendor, filter)
      toast(`${vendorLabel(vendor)} model selection is now managed here`, 'success')
      await loadModels()
    },
    [loadModels, toast],
  )

  /** What the key modal did. The catalogue and the vendor block are reloaded
   *  in every case: all three outcomes change one or both. */
  const handleVendorOutcome = useCallback(
    (outcome: Outcome) => {
      const vendor = managing?.vendor
      const name = vendor ? vendorLabel(vendor) : 'The vendor'
      if (outcome.kind === 'installed') {
        toast(
          outcome.admitted.length === 1
            ? `${name} key stored — 1 model will appear shortly`
            : `${name} key stored — ${outcome.admitted.length} models will appear shortly`,
          'success',
        )
        // Non-fatal: the key is in. Said once, as its own line, so it is not
        // mistaken for a failed install.
        for (const warning of outcome.warnings) toast(warning, 'error')
        // A key replaces whatever the last removal said about this vendor.
        if (vendor) setRemovalWarnings((prev) => ({ ...prev, [vendor]: [] }))
      } else if (outcome.kind === 'removed') {
        toast(`${name} key removed — your model selection is kept`, 'success')
        // Not toasted: they belong beside the "Removing" card for as long as
        // the removal takes, and a toast is gone in seconds.
        if (vendor) setRemovalWarnings((prev) => ({ ...prev, [vendor]: outcome.warnings }))
      }
      void loadModels()
    },
    [loadModels, managing, toast],
  )

  /**
   * The policy write the modal makes: the whole row, with only the rung
   * changed. It is a PUT, so the two fields it is not touching have to be
   * carried through — dropping them here would silently clear an allowlist as
   * a side effect of enabling a model.
   */
  const handleVendorPolicy = useCallback(
    async (residencyValue: Residency) => {
      const updated = await updateRoutingPolicy({
        residency: residencyValue,
        allowed_models: policy?.allowed_models ?? null,
        default_model: policy?.default_model ?? null,
      })
      setPolicy(updated)
      void loadPrincipals()
    },
    [policy, loadPrincipals],
  )

  /** Enables one model for the modal. Throws with the API's own message so a
   *  partial run can name exactly which model failed and why. */
  const handleVendorEnable = useCallback(async (model: AdminModel) => {
    const updated = await toggleModel(model.id, true)
    setModels((prev) => prev.map((m) => (m.id === updated.id ? updated : m)))
  }, [])

  /** The same test the policy card uses to mark itself restricted. On the
   *  catalogue tab it is the reason a row can be enabled and still refused, so
   *  it is said there too rather than only on the tab that owns it. */
  const policyRestricted = orgPolicyRestricted(policy)

  /**
   * Whether a key may be typed into this page at all.
   *
   * The api refuses one over plain HTTP and says so up front on the models
   * response; an api that predates the field says nothing, and the browser's
   * own isSecureContext answers instead. Either way the decision is made once,
   * here, and the card and the modal read the same answer — one of them
   * offering a form the other would refuse is the state worth preventing.
   */
  const keyTransportAllowed = canInstallKeyOverTransport(keyTransportOk, window.isSecureContext)
  /** And which of the two faults it is when it is not allowed — the page's
   *  scheme or the api's own setting. Read from the same three facts, in the
   *  same place, so the card and the modal never diagnose it differently. */
  const keyTransportFault = keyTransportHelp(
    keyTransportOk,
    window.isSecureContext,
    window.location.hostname,
  )

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
        <Tab id="prices" active={tab === 'prices'} onClick={() => selectTab('prices')}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <line x1="12" y1="1" x2="12" y2="23" />
            <path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" />
          </svg>
          Prices
        </Tab>
      </TabList>

      <TabPanel id="models" scope={TAB_SCOPE} active={tab === 'models'}>
        <ModelsTab
          models={models}
          filtered={filtered}
          providerGroups={providerGroups}
          providers={providers}
          providerFeatures={providerFeatures}
          transportOk={keyTransportAllowed}
          transportHelp={keyTransportFault}
          removalWarnings={removalWarnings}
          firstRunVendors={firstRunVendors}
          enableableVendors={enableableVendors}
          loading={loading}
          search={search}
          filter={filter}
          togglingIds={togglingIds}
          residency={residency}
          allowedModels={allowedModels}
          totalEnabled={totalEnabled}
          totalUnreachable={totalUnreachable}
          totalAwsOptIn={totalAwsOptIn}
          policyRestricted={policyRestricted}
          onGoToTab={goToTab}
          onSearch={setSearch}
          onFilter={setFilter}
          onToggle={handleToggle}
          onRename={handleRename}
          onReviewAndEnable={setEnablingVendor}
          onManageVendor={(vendor, mode) => setManaging({ vendor, mode })}
          onManageBilling={(vendor, mode) => setManagingBilling({ vendor, mode })}
          onCheckAgain={() => void loadModels()}
          onAdoptFilter={handleAdoptFilter}
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

      {/* Self-contained: its own reads and its two small modals. Those portal
          to <body>; the Tabs seam note applies (a modal's focus trap blocks
          every way of switching tabs while it is open). */}
      <TabPanel id="prices" scope={TAB_SCOPE} active={tab === 'prices'}>
        <PricesTab />
      </TabPanel>

      {/* Outside every TabPanel. A modal renders into a portal, so putting it
          inside a panel would leave it mounted-but-hidden when the panel is —
          and a dialog that exists in a hidden subtree is a focus trap nobody
          can see. Its state names the vendor, so the open tab is irrelevant. */}
      {enablingVendor !== null && (
        <VendorEnableModal
          open
          vendor={enablingVendor}
          models={enablingModels}
          policy={{
            residency,
            allowed_models: allowedModels,
            default_model: policy?.default_model ?? null,
          }}
          onClose={() => setEnablingVendor(null)}
          onSavePolicy={handleVendorPolicy}
          onEnable={handleVendorEnable}
          onDone={() => void loadModels()}
        />
      )}

      {/* Outside every TabPanel for the same reason as the modals below. */}
      {pendingDisable !== null && (
        <Modal
          open
          onClose={() => setPendingDisable(null)}
          title={`Disable ${pendingDisable.model.display_name ?? formatModelName(pendingDisable.model.model_id)}?`}
          description={`${pendingDisable.refs.length === 1 ? 'An alias uses' : 'Aliases use'} ${pendingDisable.model.model_id}. Turning it off changes what happens to messages sent to ${pendingDisable.refs.length === 1 ? 'it' : 'them'}:`}
        >
          <ul className={styles.refList}>
            {pendingDisable.refs.map((r) => (
              <li key={`${r.alias}/${r.route ?? ''}`}>
                <code className={styles.refId}>{r.alias}</code>
                {r.route === null ? (
                  ' → '
                ) : (
                  <>
                    {r.isDefault ? ' — default route ' : ' — route '}
                    <code className={styles.refId}>{r.route}</code>
                    {': '}
                  </>
                )}
                <span className={r.outcome === 'refused' ? styles.refRefused : undefined}>
                  {aliasReferenceConsequence(r)}
                </span>
              </li>
            ))}
          </ul>
          <div className={styles.confirmFooter}>
            <Button variant="ghost" onClick={() => setPendingDisable(null)}>
              Cancel
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                const { model } = pendingDisable
                setPendingDisable(null)
                void applyToggle(model)
              }}
            >
              Disable anyway
            </Button>
          </div>
        </Modal>
      )}

      {/* Beside the enable modal and for the same reason: a dialog lives in a
          portal, so one rendered inside a TabPanel would stay mounted in a
          hidden subtree when the panel closed — a focus trap with nothing on
          screen. Its own state names the vendor and the errand, so which tab
          is open has nothing to do with it. */}
      {managing !== null && (
        <VendorInstallModal
          open
          vendor={managing.vendor}
          mode={managing.mode}
          currentModelIds={managingModels.map((m) => m.model_id)}
          currentFilter={
            providers.find((p) => p.name === managing.vendor)?.filter ?? EMPTY_FILTER
          }
          enabledModelIds={managingModels.filter((m) => m.enabled).map((m) => m.model_id)}
          currentWorkspaceId={
            providers.find((p) => p.name === managing.vendor)?.workspace_id ?? null
          }
          transportOk={keyTransportAllowed}
          transportHelp={keyTransportFault}
          onClose={() => setManaging(null)}
          onDone={handleVendorOutcome}
        />
      )}

      {/* Same placement rule as the key modal above. */}
      {managingBilling !== null && (
        <VendorBillingKeyModal
          open
          vendor={managingBilling.vendor}
          mode={managingBilling.mode}
          transportOk={keyTransportAllowed}
          onClose={() => setManagingBilling(null)}
          onDone={(kind) => {
            const name = vendorLabel(managingBilling.vendor)
            toast(
              kind === 'installed'
                ? `${name} billing key stored — reports are pulled on the next daily run, or with Pull now under Activity > Spend`
                : `${name} billing key removed — revoke it in ${name}'s console too`,
              'success',
            )
            void loadModels()
          }}
        />
      )}
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
  providers: ProviderStatus[]
  providerFeatures: string[]
  transportOk: boolean
  transportHelp: KeyTransportHelp | null
  removalWarnings: Partial<Record<VendorName, string[]>>
  firstRunVendors: Set<VendorName>
  enableableVendors: Set<VendorName>
  loading: boolean
  search: string
  filter: Filter
  togglingIds: Set<string>
  residency: Residency
  allowedModels: string[] | null
  totalEnabled: number
  totalUnreachable: number
  /** Rows AWS refuses until the account's Bedrock data-retention mode is
   *  raised. Whole catalogue, like the other counts. */
  totalAwsOptIn: number
  policyRestricted: boolean
  onGoToTab: (tab: TabKey) => void
  onSearch: (value: string) => void
  onFilter: (value: Filter) => void
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
  onReviewAndEnable: (vendor: VendorName) => void
  onManageVendor: (vendor: VendorName, mode: VendorInstallMode) => void
  onManageBilling: (vendor: VendorName, mode: BillingKeyMode) => void
  onCheckAgain: () => void
  onAdoptFilter: (vendor: VendorName, filter: string[]) => Promise<void>
}

function ModelsTab({
  models,
  filtered,
  providerGroups,
  providers,
  providerFeatures,
  transportOk,
  transportHelp,
  removalWarnings,
  firstRunVendors,
  enableableVendors,
  loading,
  search,
  filter,
  togglingIds,
  residency,
  allowedModels,
  totalEnabled,
  totalUnreachable,
  totalAwsOptIn,
  policyRestricted,
  onGoToTab,
  onSearch,
  onFilter,
  onToggle,
  onRename,
  onReviewAndEnable,
  onManageVendor,
  onManageBilling,
  onCheckAgain,
  onAdoptFilter,
}: ModelsTabProps) {
  /** Enabled models that send prompts out of the account. Counted from the
   *  whole catalogue, like the other summary counts, not from the filter. */
  const egressEnabled = models.filter((m) => m.enabled && isEgress(displayClassOf(m))).length

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

      {/* Whether a vendor can be reached at all decides whether half the
          catalogue below means anything, so it is answered before it. */}
      <VendorStatusSection
        providers={providers}
        providerFeatures={providerFeatures}
        transportOk={transportOk}
        transportHelp={transportHelp}
        removalWarnings={removalWarnings}
        loading={loading}
        enableableVendors={enableableVendors}
        onManage={onManageVendor}
        onReviewAndEnable={onReviewAndEnable}
        onCheckAgain={onCheckAgain}
        onAdoptFilter={onAdoptFilter}
        onManageBilling={onManageBilling}
      />

      {/* Named for anyone navigating by heading, like the other two tabs; the
          visible name is the tab itself. */}
      <h2 className={styles.srOnly}>Available models</h2>

      {/* One standing limit on every model below, whoever serves it — said once
          here rather than on each provider's card, which made a product-wide
          refusal read as something one vendor does. */}
      <p className={styles.catalogueNote}>{UNSUPPORTED_CONTENT_NOTE}</p>

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
          {/* Counted separately and said plainly. An administrator asked "how
              much of this leaves our account?" should not have to read every
              row's badge to find out. */}
          {egressEnabled > 0 && (
            <span className={styles.summaryFlag}>
              <EgressGlyph />
              {egressEnabled} {egressEnabled === 1 ? 'sends prompts' : 'send prompts'} outside your
              cloud account
            </span>
          )}
          {totalAwsOptIn > 0 && (
            <span className={styles.summaryFlag}>
              <OptInGlyph />
              {awsOptInCountText(totalAwsOptIn)}
            </span>
          )}
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
            firstRun={isVendorProvider(provider) && firstRunVendors.has(provider)}
            onReviewAndEnable={onReviewAndEnable}
            onToggle={onToggle}
            onRename={onRename}
          />
        ))
      )}
    </>
  )
}
