import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
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
import { disambiguate, formatModelName, parseBedrockId, scopeLabel, type BedrockScope } from '../lib/formatModelName'
import {
  UNKNOWN_CLASS_HINT,
  UNREACHABLE_HINT,
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
  Select,
  Tab,
  TabList,
  TabPanel,
  Table,
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
import { RETIRED_LABEL, RETIRED_STATUS_HINT, RETIRED_TOGGLE_TITLE, isRetired } from '../lib/modelRetired'
import { Skeleton } from '../components/ui/Skeleton'
import { AccessOverviewSection } from './AccessOverviewSection'
import { RoutingPolicyCard, type OrgPolicyDraft } from './RoutingPolicyCard'
import { SystemPromptCard } from './SystemPromptCard'
import { VendorEnableModal } from './VendorEnableModal'
import { VendorInstallModal, type Outcome, type VendorInstallMode } from './VendorInstallModal'
import { VendorStatusSection, stateCopy } from './VendorStatusSection'
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
type TabKey = 'models' | 'vendors' | 'policy' | 'prices' | 'prompt'
/** Prefix for this page's tab/panel DOM ids, so another tab set on the same
 *  page could never share an id with these. */
const TAB_SCOPE = 'models-page'

function tabFromParam(value: string | null): TabKey {
  return value === 'vendors' || value === 'policy' || value === 'prices' || value === 'prompt' ? value : 'models'
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
  | { kind: 'retired'; hint: string }
  | { kind: 'unreachable'; hint: string }
  | null

function rowFlag(
  m: AdminModel,
  residency: Residency,
  allowedModels: string[] | null,
): RowFlag {
  // First, off or on: a retired model fails every chat whatever the policy says,
  // and no policy change or backend recovery brings it back.
  if (isRetired(m)) return { kind: 'retired', hint: RETIRED_STATUS_HINT }

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
  blockedReason,
}: {
  enabled: boolean
  loading: boolean
  onToggle: () => void
  modelName: string
  /** Why it cannot be turned on; set means the switch is inert. */
  blockedReason?: string
}) {
  return (
    <button
      className={`${styles.toggle} ${enabled ? styles.toggleEnabled : ''}`}
      onClick={onToggle}
      disabled={loading || !!blockedReason}
      title={blockedReason}
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
  const shown = model.display_name ?? formatModelName(model.model_id, model.provider)

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
        placeholder={formatModelName(model.model_id, model.provider)}
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
  /** The rows to list: what search and the chip left. */
  models: AdminModel[]
  /** The provider's visible catalogue, for the header's counts, so they agree
   *  with the summary line whatever the search box says. */
  allModels: AdminModel[]
  togglingIds: Set<string>
  residency: Residency
  allowedModels: string[] | null
  firstRun: boolean
  /** True while the search box or the filter is away from its default: every
   *  maker group is then open, because the person is looking for something. */
  searching: boolean
  onReviewAndEnable: (vendor: VendorName) => void
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
}

/** One model as the catalogue shows it. On Bedrock the same model is offered
 *  under more than one routing scope ("us.", "global."), each its own id and
 *  its own switch at the gateway; here they are one row with a scope choice,
 *  because an administrator decides "Claude Sonnet 4.5: yes or no" once, and
 *  where AWS may route it is the second question, not a second model. */
interface LogicalRow {
  key: string
  /** The maker of the weights ("anthropic"), for grouping. '' when unknown. */
  maker: string
  scopes: { scope: BedrockScope | null; model: AdminModel }[]
}

/** Groups a provider's rows into logical rows, makers in alphabetical order.
 *  Ordering is by id, never by state, so a row does not move when its switch
 *  is flipped. */
function logicalRows(provider: string, models: AdminModel[]): Map<string, LogicalRow[]> {
  const byKey = new Map<string, LogicalRow>()
  for (const m of models) {
    const parsed = provider === 'bedrock' ? parseBedrockId(m.model_id) : null
    const key = parsed ? parsed.unscoped : m.model_id
    let row = byKey.get(key)
    if (!row) {
      row = { key, maker: parsed?.maker ?? '', scopes: [] }
      byKey.set(key, row)
    }
    row.scopes.push({ scope: parsed?.scope ?? null, model: m })
  }
  const groups = new Map<string, LogicalRow[]>()
  for (const row of byKey.values()) {
    // US first, then the rest alphabetically: the in-region scope is the
    // default choice, so it is the first option too.
    row.scopes.sort((a, b) => (a.scope === 'us' ? -1 : b.scope === 'us' ? 1 : (a.scope ?? '').localeCompare(b.scope ?? '')))
    const list = groups.get(row.maker) ?? []
    list.push(row)
    groups.set(row.maker, list)
  }
  for (const list of groups.values()) {
    list.sort((a, b) => {
      // Retired-and-off rows sink to the bottom of their group.
      const ar = a.scopes.every((x) => isRetired(x.model) && !x.model.enabled)
      const br = b.scopes.every((x) => isRetired(x.model) && !x.model.enabled)
      if (ar !== br) return ar ? 1 : -1
      return a.key.localeCompare(b.key)
    })
  }
  return new Map([...groups.entries()].sort(([a], [b]) => a.localeCompare(b)))
}

/** Which scope a row shows: the one that is on; failing that, US; failing
 *  that, the first. The person can change it while the row is off. */
function defaultScope(row: LogicalRow): string {
  const on = row.scopes.filter((x) => x.model.enabled)
  if (on.length === 1) return on[0].model.id
  // Off everywhere: prefer a scope that can actually be turned on.
  const live = row.scopes.filter((x) => !isRetired(x.model))
  const pool = live.length > 0 ? live : row.scopes
  const us = pool.find((x) => x.scope === 'us')
  return (us ?? pool[0]).model.id
}

/** The scope a row shows. With exactly one scope on, that is the one: a
 *  remembered choice (or the US default) must never put the switch on a
 *  scope other than the one that is live. Otherwise the person's choice. */
function shownScopeId(row: LogicalRow, chosen: string | undefined): string {
  const on = row.scopes.filter((x) => x.model.enabled)
  if (on.length === 1) return on[0].model.id
  if (chosen && row.scopes.some((x) => x.model.id === chosen)) return chosen
  return defaultScope(row)
}

/** The one badge a row shows, or null when there is nothing wrong with it. */
function FlagBadge({ flag, enabled, unreachable }: { flag: RowFlag; enabled: boolean; unreachable: boolean }) {
  if (flag?.kind === 'blocked') {
    return (
      <Badge variant="warning" bordered>
        <LockGlyph />
        Blocked by policy
      </Badge>
    )
  }
  if (flag?.kind === 'disabled_by_policy') {
    return (
      <Badge variant="neutral" bordered>
        <LockGlyph />
        Blocked by policy
      </Badge>
    )
  }
  if (flag?.kind === 'aws_opt_in') {
    return (
      <Badge variant={enabled ? 'warning' : 'neutral'} bordered>
        <OptInGlyph />
        {AWS_OPT_IN_LABEL}
      </Badge>
    )
  }
  if (flag?.kind === 'retired') {
    return (
      <Badge variant={enabled ? 'warning' : 'neutral'} bordered>
        <UnreachableGlyph />
        {RETIRED_LABEL}
      </Badge>
    )
  }
  if (flag?.kind === 'unknown') {
    return (
      <Badge variant="neutral" bordered dashed>
        <UnknownGlyph />
        Not reported
      </Badge>
    )
  }
  if (unreachable) {
    return (
      <Badge variant="warning" bordered>
        <UnreachableGlyph />
        Not reachable
      </Badge>
    )
  }
  return null
}

function ProviderSection({
  provider,
  models,
  allModels,
  togglingIds,
  residency,
  allowedModels,
  firstRun,
  searching,
  onReviewAndEnable,
  onToggle,
  onRename,
}: ProviderSectionProps) {
  const [collapsed, setCollapsed] = useState(false)
  /** Maker groups the person has opened or closed by hand. Anything not here
   *  follows the rule: open if it has a model that is on, or while searching. */
  const [groupOverride, setGroupOverride] = useState<Map<string, boolean>>(() => new Map())
  const [openDetails, setOpenDetails] = useState<Set<string>>(() => new Set())
  const [chosenScope, setChosenScope] = useState<Map<string, string>>(() => new Map())

  const groups = useMemo(() => logicalRows(provider, models), [provider, models])
  // Bedrock only (other providers' ids are not in that shape), and by unscoped
  // id: the us. and global. twins are one model and must not "collide".
  const names = useMemo(
    () =>
      provider === 'bedrock'
        ? disambiguate([...new Set(models.map((m) => parseBedrockId(m.model_id).unscoped))])
        : new Map<string, string>(),
    [provider, models],
  )
  const allGroups = useMemo(() => logicalRows(provider, allModels), [provider, allModels])
  const logicalCount = useMemo(() => [...allGroups.values()].reduce((n, g) => n + g.length, 0), [allGroups])
  const enabledLogical = useMemo(
    () => [...allGroups.values()].reduce((n, g) => n + g.filter((r) => r.scopes.some((x) => x.model.enabled)).length, 0),
    [allGroups],
  )
  const unreachableCount = useMemo(
    () =>
      [...allGroups.values()].reduce(
        (n, g) => n + g.filter((r) => r.scopes.some((x) => !isReachable(x.model) && !isRetired(x.model))).length,
        0,
      ),
    [allGroups],
  )

  /** The class of this provider's models. One badge on the header says it for
   *  every row beneath; a row that differs from it gets its own badge. */
  const headerClass: DisplayClass = useMemo(() => {
    const classes = new Set(models.map((m) => displayClassOf(m)))
    return classes.size === 1 ? [...classes][0] : 'unknown'
  }, [models])

  const isGroupOpen = (maker: string, rows: LogicalRow[]) => {
    const forced = groupOverride.get(maker)
    if (forced !== undefined) return forced
    return searching || rows.some((r) => r.scopes.some((x) => x.model.enabled))
  }
  const allOpen = [...groups.entries()].every(([maker, rows]) => isGroupOpen(maker, rows))
  const setAllGroups = (open: boolean) => {
    setGroupOverride(new Map([...groups.keys()].map((maker) => [maker, open])))
  }

  const renderRow = (row: LogicalRow) => {
    const selectedId = shownScopeId(row, chosenScope.get(row.key))
    const selected = row.scopes.find((x) => x.model.id === selectedId) ?? row.scopes[0]
    const m = selected.model
    const onCount = row.scopes.filter((x) => x.model.enabled).length
    const displayName = m.display_name ?? names.get(parseBedrockId(m.model_id).unscoped) ?? formatModelName(m.model_id, m.provider)
    const flag = rowFlag(m, residency, allowedModels)
    const unreachable = !isReachable(m) && flag?.kind !== 'retired'
    // A stripe only when a chat to this row is failing now: on, and refused.
    const failing =
      m.enabled && (flag?.kind === 'blocked' || flag?.kind === 'aws_opt_in' || flag?.kind === 'retired' || unreachable)
    const retiredOff = row.scopes.every((x) => isRetired(x.model) && !x.model.enabled)
    const detailsOpen = openDetails.has(row.key)
    const detailsId = `model-details-${provider}-${row.key.replace(/[^a-zA-Z0-9]/g, '-')}`
    const cls = displayClassOf(m)
    const toggleName = row.scopes.length > 1 ? `${displayName} (${scopeLabel(selected.scope)})` : displayName
    return (
      <Fragment key={row.key}>
        <tr className={`${failing ? styles.rowFlagged : ''} ${retiredOff ? styles.rowRetiredOff : ''}`}>
          <td className={styles.detailsCol}>
            <button
              type="button"
              className={styles.detailsBtn}
              aria-expanded={detailsOpen}
              aria-controls={detailsId}
              aria-label={`Details for ${displayName}`}
              onClick={() =>
                setOpenDetails((prev) => {
                  const next = new Set(prev)
                  if (next.has(row.key)) next.delete(row.key)
                  else next.add(row.key)
                  return next
                })
              }
            >
              <svg
                className={`${styles.detailsChevron} ${detailsOpen ? styles.detailsChevronOpen : ''}`}
                width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"
              >
                <polyline points="9 18 15 12 9 6" />
              </svg>
            </button>
          </td>
          <td>
            <div className={styles.modelCell} title={m.model_id}>
              <DisplayNameCell model={m} onRename={onRename} />
              {cls !== headerClass && (
                <Badge variant={backendClassVariant(cls)} bordered dashed={cls === 'unknown'} title={backendClassTooltip(cls)}>
                  <ClassGlyph cls={cls} />
                  {backendClassLabel(cls)}
                </Badge>
              )}
            </div>
          </td>
          <td className={styles.scopeCol}>
            {provider !== 'bedrock' ? null : row.scopes.length > 1 && onCount <= 1 ? (
              <Select
                className={styles.scopeSelect}
                aria-label={`Routing scope for ${displayName}`}
                value={selected.model.id}
                disabled={m.enabled || togglingIds.has(m.id)}
                title={m.enabled ? 'Turn the model off to change where AWS may route it.' : 'Where AWS may route requests for this model.'}
                onChange={(e) => setChosenScope((prev) => new Map(prev).set(row.key, e.target.value))}
              >
                {row.scopes.map((x) => (
                  // The status badge beside the select says why a scope is
                  // refused; the option only says when it cannot be chosen.
                  <option key={x.model.id} value={x.model.id} disabled={isRetired(x.model)}>
                    {scopeLabel(x.scope)}
                    {x.scope === 'us' ? ' regions' : ''}
                    {isRetired(x.model) ? ' (retired)' : ''}
                  </option>
                ))}
              </Select>
            ) : (
              <span className={styles.scopeText} title={onCount > 1 ? 'Both scopes are on; see details.' : undefined}>
                {onCount > 1 ? 'Both' : scopeLabel(selected.scope)}
              </span>
            )}
          </td>
          <td className={styles.flagCol}>
            <FlagBadge flag={flag} enabled={m.enabled} unreachable={unreachable} />
          </td>
          <td className={styles.statusCol}>
            {onCount > 1 ? (
              // Both scopes are on; one switch here would turn off only the
              // scope it happened to show. Each scope has its own in details.
              <span className={styles.scopeText} title="Both scopes are on. Open details to turn either off.">
                Both on
              </span>
            ) : (
              <ToggleSwitch
                enabled={m.enabled}
                loading={togglingIds.has(m.id)}
                onToggle={() => onToggle(m)}
                modelName={toggleName}
                blockedReason={isRetired(m) && !m.enabled ? RETIRED_TOGGLE_TITLE : undefined}
              />
            )}
          </td>
        </tr>
        {detailsOpen && (
          <tr id={detailsId}>
            <td colSpan={5} className={styles.detailCell}>
              {flag && <p className={styles.detailHint}>{flag.hint}</p>}
              {!flag && unreachable && (
                <p className={styles.detailHint}>
                  The gateway has not seen this model's backend for a few minutes. It stays configured; chats to it fail until it is back.
                </p>
              )}
              <div className={styles.detailGrid}>
                <span className={styles.detailLabel}>{row.scopes.length > 1 ? 'Model IDs' : 'Model ID'}</span>
                <div className={styles.detailIds}>
                  {row.scopes.map((x) => {
                    const f = rowFlag(x.model, residency, allowedModels)
                    const u = !isReachable(x.model) && f?.kind !== 'retired'
                    return (
                      <div key={x.model.id} className={styles.detailIdRow}>
                        {row.scopes.length > 1 && <span className={styles.scopeText}>{scopeLabel(x.scope)}</span>}
                        <code className={styles.modelId}>{x.model.model_id}</code>
                        <CopyButton value={x.model.model_id} label="Copy" />
                        {row.scopes.length > 1 && <FlagBadge flag={f} enabled={x.model.enabled} unreachable={u} />}
                        {row.scopes.length > 1 && onCount > 1 && (
                          <ToggleSwitch
                            enabled={x.model.enabled}
                            loading={togglingIds.has(x.model.id)}
                            onToggle={() => onToggle(x.model)}
                            modelName={`${displayName} (${scopeLabel(x.scope)})`}
                            blockedReason={isRetired(x.model) && !x.model.enabled ? RETIRED_TOGGLE_TITLE : undefined}
                          />
                        )}
                      </div>
                    )
                  })}
                </div>
                <span className={styles.detailLabel}>Hosting</span>
                <span>
                  {backendClassLabel(cls)}
                  {backendClassTooltip(cls) ? ` — ${backendClassTooltip(cls)}` : ''}
                </span>
                <span className={styles.detailLabel}>Name</span>
                <span>{m.display_name ? 'Renamed' : 'Generated from the model ID'}</span>
                <span className={styles.detailLabel}>Discovered</span>
                <span>{new Date(m.discovered_at).toLocaleDateString()}</span>
                <span className={styles.detailLabel}>Updated</span>
                <span>{new Date(m.updated_at).toLocaleDateString()}</span>
              </div>
            </td>
          </tr>
        )}
      </Fragment>
    )
  }

  const hasMakers = provider === 'bedrock' && groups.size > 1

  return (
    <div className={styles.providerSection}>
      <div className={styles.providerCard}>
        <div className={styles.providerHeader}>
          <div
            className={styles.providerHeaderLeft}
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
            <span className={styles.providerName}>{providerLabel(provider)}</span>
            {/* Where everything under this header runs: said once here, not on
                every row. A row that differs carries its own badge. */}
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
              {enabledLogical} of {logicalCount} enabled
            </span>
            {unreachableCount > 0 && (
              <span className={styles.providerUnreachable}>{unreachableCount} not reachable</span>
            )}
          </div>
          {hasMakers && !collapsed && (
            <div className={styles.providerHeaderRight}>
              <button type="button" className={styles.expandAll} onClick={() => setAllGroups(!allOpen)}>
                {allOpen ? 'Collapse all' : 'Expand all'}
              </button>
            </div>
          )}
        </div>

        <div className={collapsed ? styles.providerBodyCollapsed : styles.providerBody}>
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
              <Button size="sm" variant="secondary" onClick={() => onReviewAndEnable(provider as VendorName)}>
                Review and enable
              </Button>
            </div>
          )}

          <Table label={`${providerLabel(provider)} models`}>
            <thead>
              <tr>
                <th className={styles.detailsCol}>
                  <span className={styles.srOnly}>Details</span>
                </th>
                <th>Model</th>
                <th className={styles.scopeCol}>Scope</th>
                <th className={styles.flagCol}>Status</th>
                <th className={styles.statusCol}>
                  <span className={styles.srOnly}>Enabled</span>
                </th>
              </tr>
            </thead>
            {[...groups.entries()].map(([maker, rows]) => {
              const open = !hasMakers || isGroupOpen(maker, rows)
              const on = rows.filter((r) => r.scopes.some((x) => x.model.enabled)).length
              const optIn = rows.filter((r) => r.scopes.some((x) => needsAwsOptIn(x.model))).length
              const retired = rows.filter((r) => r.scopes.every((x) => isRetired(x.model))).length
              // Said on the closed group, so a problem inside it is not hidden
              // behind the chevron.
              const blocked = rows.filter((r) =>
                r.scopes.some((x) => {
                  const k = rowFlag(x.model, residency, allowedModels)?.kind
                  return k === 'blocked' || k === 'disabled_by_policy'
                }),
              ).length
              const down = rows.filter((r) => r.scopes.some((x) => !isReachable(x.model) && !isRetired(x.model))).length
              return (
                <tbody key={maker || '_other'}>
                  {hasMakers && (
                    <tr className={styles.groupRow}>
                      <th colSpan={5} scope="rowgroup">
                        <button
                          type="button"
                          className={styles.groupBtn}
                          aria-expanded={open}
                          onClick={() => setGroupOverride((prev) => new Map(prev).set(maker, !open))}
                        >
                          <svg
                            className={`${styles.groupChevron} ${open ? styles.groupChevronOpen : ''}`}
                            viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"
                          >
                            <polyline points="9 18 15 12 9 6" />
                          </svg>
                          <span className={styles.groupName}>{bedrockVendorHeading(maker)}</span>
                          <span className={styles.groupMeta}>
                            {rows.length} {rows.length === 1 ? 'model' : 'models'}
                            {on > 0 ? ` · ${on} enabled` : ''}
                            {retired > 0 ? ` · ${retired} retired` : ''}
                            {blocked > 0 ? ` · ${blocked} blocked by policy` : ''}
                            {down > 0 ? ` · ${down} not reachable` : ''}
                          </span>
                          {optIn > 0 && (
                            <Badge variant="neutral" bordered>
                              <OptInGlyph />
                              {optIn} {optIn === 1 ? 'needs' : 'need'} AWS opt-in
                            </Badge>
                          )}
                        </button>
                      </th>
                    </tr>
                  )}
                  {open && rows.map(renderRow)}
                </tbody>
              )
            })}
          </Table>
        </div>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Loading skeleton: fixed heights, so nothing moves when the rows land.
// ---------------------------------------------------------------------------

const SKELETON_NAME_WIDTHS = ['38%', '52%', '30%', '45%', '34%', '48%']

function LoadingSkeleton() {
  return (
    <div className={styles.providerSection} aria-busy="true">
      <span className={styles.srOnly}>Loading models</span>
      <div className={styles.providerCard}>
        <div className={styles.providerHeader} style={{ cursor: 'default' }}>
          <div className={styles.providerHeaderLeft}>
            <Skeleton width="120px" height="16px" />
            <Skeleton width="100px" height="18px" borderRadius="var(--radius-full)" />
          </div>
        </div>
        <div className={styles.providerBody}>
          {SKELETON_NAME_WIDTHS.map((w, i) => (
            <div key={i} className={styles.skeletonFixedRow}>
              <Skeleton width="14px" height="14px" />
              <Skeleton width={w} height="14px" />
              <Skeleton width="48px" height="14px" />
              {i === 1 || i === 4 ? <Skeleton width="88px" height="18px" borderRadius="var(--radius-full)" /> : <span style={{ width: 88 }} />}
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
  // Retired models are history, not choices; they are listed on request. One
  // that is still on is always shown, because chats to it are failing.
  const [showRetired, setShowRetired] = useState(false)
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
        // Only when the api reported it: an older api would ignore it, and the
        // card would show a strict mode that is not stored.
        ...(draft.params_mode !== null ? { params_mode: draft.params_mode } : {}),
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
    const refs = aliasReferencesTo(model.model_id, current, residency)
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
        `${model.display_name ?? formatModelName(model.model_id, model.provider)} ${newEnabled ? 'enabled' : 'disabled'}`,
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
    // Enabled/Disabled and search decide per logical model, not per id: a model
    // on in one routing scope and off in the other is one row, and filtering
    // must not split it into a half that contradicts the chip counts.
    const keyOf = (m: AdminModel) =>
      `${m.provider}:${m.provider === 'bedrock' ? parseBedrockId(m.model_id).unscoped : m.model_id}`
    const q = search.trim().toLowerCase()
    const keep = new Set<string>()
    const enabledKeys = new Set<string>()
    for (const m of models) if (m.enabled) enabledKeys.add(keyOf(m))
    for (const m of models) {
      const k = keyOf(m)
      if (filter === 'enabled' && !enabledKeys.has(k)) continue
      if (filter === 'disabled' && enabledKeys.has(k)) continue
      if (
        q &&
        !(
          m.model_id.toLowerCase().includes(q) ||
          (m.display_name?.toLowerCase().includes(q) ?? false) ||
          formatModelName(m.model_id, m.provider).toLowerCase().includes(q)
        )
      )
        continue
      keep.add(k)
    }
    return models.filter((m) => keep.has(keyOf(m)) && (showRetired || !(isRetired(m) && !m.enabled)))
  }, [models, filter, search, showRetired])

  /** The catalogue as the page shows it: retired-and-off rows are out unless
   *  asked for. Every count on the page is taken from this one list, by model
   *  rather than by id (a Bedrock model offered in two routing scopes is one
   *  choice), so the summary, the chips, the tab badge and the section
   *  headers can never disagree. Search and the Enabled/Disabled chip narrow
   *  what is listed, never what is counted. */
  const visible = useMemo(
    () => (showRetired ? models : models.filter((m) => !(isRetired(m) && !m.enabled))),
    [models, showRetired],
  )
  const logical = useMemo(() => {
    const keys = new Map<string, { enabled: boolean; optIn: boolean; unreachable: boolean }>()
    for (const m of visible) {
      const key = `${m.provider}:${m.provider === 'bedrock' ? parseBedrockId(m.model_id).unscoped : m.model_id}`
      const cur = keys.get(key) ?? { enabled: false, optIn: false, unreachable: false }
      keys.set(key, {
        enabled: cur.enabled || m.enabled,
        optIn: cur.optIn || needsAwsOptIn(m),
        unreachable: cur.unreachable || (!isReachable(m) && !isRetired(m)),
      })
    }
    const rows = [...keys.values()]
    const enabled = rows.filter((r) => r.enabled).length
    // Retired-and-off models, counted from the whole catalogue: the number
    // the "Show retired" control would add.
    const retiredKeys = new Map<string, boolean>()
    for (const m of models) {
      const key = `${m.provider}:${m.provider === 'bedrock' ? parseBedrockId(m.model_id).unscoped : m.model_id}`
      retiredKeys.set(key, (retiredKeys.get(key) ?? true) && isRetired(m) && !m.enabled)
    }
    const retiredOff = [...retiredKeys.values()].filter(Boolean).length
    return {
      total: rows.length,
      enabled,
      disabled: rows.length - enabled,
      retiredOff,
      optIn: rows.filter((r) => r.optIn).length,
      unreachable: rows.filter((r) => r.unreachable).length,
    }
  }, [models, visible])

  /** The visible catalogue by provider, for the section headers' counts. */
  const visibleByProvider = useMemo(() => {
    const groups = new Map<string, AdminModel[]>()
    for (const m of visible) {
      if (!groups.has(m.provider)) groups.set(m.provider, [])
      groups.get(m.provider)!.push(m)
    }
    return groups
  }, [visible])

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

  /** A key that is installed and not working, or a filter that matches
   *  nothing: the tab says so before anyone opens it. */
  const vendorsNeedAttention = providers.some((p) =>
    p.state === 'auth_failed' || p.state === 'unreachable' || p.state === 'filter_missing' || p.state === 'filter_matched_nothing',
  )

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
          count={logical.enabled > 0 ? logical.enabled : undefined}
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
        <Tab id="vendors" active={tab === 'vendors'} onClick={() => selectTab('vendors')}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
          </svg>
          Vendor keys
          {vendorsNeedAttention && (
            <span className={styles.tabWarn} title="A vendor key needs attention">
              <UnreachableGlyph />
              <span className={styles.srOnly}>needs attention</span>
            </span>
          )}
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
        <Tab id="prompt" active={tab === 'prompt'} onClick={() => selectTab('prompt')}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
          </svg>
          System prompt
        </Tab>
      </TabList>

      <TabPanel id="models" scope={TAB_SCOPE} active={tab === 'models'}>
        <ModelsTab
          models={models}
          filtered={filtered}
          providerGroups={providerGroups}
          providers={providers}
          firstRunVendors={firstRunVendors}
          loading={loading}
          search={search}
          filter={filter}
          showRetired={showRetired}
          togglingIds={togglingIds}
          residency={residency}
          allowedModels={allowedModels}
          logical={logical}
          visibleByProvider={visibleByProvider}
          policyRestricted={policyRestricted}
          onGoToTab={goToTab}
          onSearch={setSearch}
          onFilter={setFilter}
          onShowRetired={setShowRetired}
          onToggle={handleToggle}
          onRename={handleRename}
          onReviewAndEnable={setEnablingVendor}
          onCheckAgain={() => void loadModels()}
        />
      </TabPanel>

      <TabPanel id="vendors" scope={TAB_SCOPE} active={tab === 'vendors'}>
        <VendorStatusSection
          providers={providers}
          providerFeatures={providerFeatures}
          transportOk={keyTransportAllowed}
          transportHelp={keyTransportFault}
          removalWarnings={removalWarnings}
          loading={loading}
          enableableVendors={enableableVendors}
          onManage={(vendor, mode) => setManaging({ vendor, mode })}
          onReviewAndEnable={setEnablingVendor}
          onCheckAgain={() => void loadModels()}
          onAdoptFilter={handleAdoptFilter}
          onManageBilling={(vendor, mode) => setManagingBilling({ vendor, mode })}
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

      <TabPanel id="prompt" scope={TAB_SCOPE} active={tab === 'prompt'}>
        <SystemPromptCard />
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

interface LogicalCounts {
  total: number
  enabled: number
  disabled: number
  /** Models every scope of which AWS has retired, and none of which is on. */
  retiredOff: number
  /** Models AWS refuses until the account's data-retention mode is raised. */
  optIn: number
  /** Models whose backend the gateway has not seen for a few minutes. */
  unreachable: number
}

interface ModelsTabProps {
  models: AdminModel[]
  filtered: AdminModel[]
  providerGroups: [string, AdminModel[]][]
  providers: ProviderStatus[]
  firstRunVendors: Set<VendorName>
  loading: boolean
  search: string
  filter: Filter
  showRetired: boolean
  togglingIds: Set<string>
  residency: Residency
  allowedModels: string[] | null
  logical: LogicalCounts
  visibleByProvider: Map<string, AdminModel[]>
  policyRestricted: boolean
  onGoToTab: (tab: TabKey) => void
  onSearch: (value: string) => void
  onFilter: (value: Filter) => void
  onShowRetired: (value: boolean) => void
  onToggle: (model: AdminModel) => void
  onRename: (model: AdminModel, displayName: string | null) => Promise<void>
  onReviewAndEnable: (vendor: VendorName) => void
  onCheckAgain: () => void
}

/** The one line the Models tab says about vendor keys. Everything else about
 *  them lives on the Vendor keys tab; this is what tells someone whether that
 *  tab needs a visit. */
function VendorStrip({ providers, onGoToTab }: { providers: ProviderStatus[]; onGoToTab: (tab: TabKey) => void }) {
  const copies = providers.map((p) => ({ p, copy: stateCopy(p, true, true) }))
  const failed = copies.find(({ p }) => p.state === 'auth_failed')
  const landing = copies.find(({ p }) => p.installing_until && new Date(p.installing_until).getTime() > Date.now())
  const warn = copies.find(({ p }) => p.state === 'unreachable' || p.state === 'filter_missing' || p.state === 'filter_matched_nothing')
  const allOff = providers.length > 0 && providers.every((p) => p.state === 'not_configured')
  const vendorModels = providers.reduce((n, p) => n + (p.configured ? p.model_count : 0), 0)

  let note: string
  let tone = ''
  if (failed) {
    note = `${vendorLabel(failed.p.name)} rejected the key darkplum holds. Requests to it are refused.`
    tone = styles.vendorStripDanger
  } else if (landing) {
    note = `Installing the ${vendorLabel(landing.p.name)} key — checking it through the gateway.`
  } else if (warn) {
    note = warn.copy.detail
    tone = styles.vendorStripWarning
  } else if (allOff) {
    note = 'Optional. Bedrock and self-hosted models work without them.'
  } else {
    note = `${vendorModels} ${vendorModels === 1 ? 'model' : 'models'} from vendor APIs in the catalogue.`
  }

  return (
    <section className={`${styles.vendorStrip} ${tone}`} aria-labelledby="vendor-strip-heading">
      <h2 id="vendor-strip-heading" className={styles.srOnly}>
        Vendor keys
      </h2>
      <ul className={styles.vendorList}>
        {copies.map(({ p, copy }) => (
          <li key={p.name}>
            <span className={styles.vendorName}>{vendorLabel(p.name)}</span>
            <Badge variant={copy.variant} bordered dashed={copy.dashed}>
              {copy.label}
            </Badge>
          </li>
        ))}
      </ul>
      <span className={styles.vendorStripNote} title={note}>
        {note}
      </span>
      <Link
        className={styles.vendorStripLink}
        to={{ search: '?tab=vendors' }}
        onClick={(e) => {
          if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
          e.preventDefault()
          onGoToTab('vendors')
        }}
      >
        Manage vendor keys
      </Link>
    </section>
  )
}

function EmptyCard({
  glyph,
  title,
  hint,
  actions,
}: {
  glyph: React.ReactNode
  title: string
  hint?: string
  actions?: React.ReactNode
}) {
  return (
    <div className={`${styles.providerSection} ${styles.emptyCard}`}>
      <div className={styles.empty}>
        {glyph}
        <span className={styles.emptyTitle}>{title}</span>
        {hint && <span className={styles.emptyHint}>{hint}</span>}
        {actions && <div className={styles.emptyActions}>{actions}</div>}
      </div>
    </div>
  )
}

const SearchGlyph = () => (
  <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <circle cx="11" cy="11" r="8" />
    <line x1="21" y1="21" x2="16.65" y2="16.65" />
  </svg>
)

const CatalogueGlyph = () => (
  <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <rect x="2" y="3" width="20" height="14" rx="2" ry="2" />
    <line x1="8" y1="21" x2="16" y2="21" />
    <line x1="12" y1="17" x2="12" y2="21" />
  </svg>
)

function ModelsTab({
  models,
  filtered,
  providerGroups,
  providers,
  firstRunVendors,
  loading,
  search,
  filter,
  showRetired,
  togglingIds,
  residency,
  allowedModels,
  logical,
  visibleByProvider,
  policyRestricted,
  onGoToTab,
  onSearch,
  onFilter,
  onShowRetired,
  onToggle,
  onRename,
  onReviewAndEnable,
  onCheckAgain,
}: ModelsTabProps) {
  /** Enabled models that send prompts out of the account. Counted from the
   *  whole catalogue, like the other summary counts, not from the filter. */
  const egressEnabled = models.filter((m) => m.enabled && isEgress(displayClassOf(m))).length
  /** An opt-in row that is on is refusing chats now; the summary then says it
   *  in the warning colour rather than as a count of things to know about. */
  const optInLive = models.some((m) => m.enabled && needsAwsOptIn(m))
  const searching = search.trim() !== '' || filter !== 'all'
  const retiredMatching =
    !showRetired && search.trim()
      ? models.filter((m) => isRetired(m) && !m.enabled && m.model_id.toLowerCase().includes(search.trim().toLowerCase())).length
      : 0

  // Matches counted by model, like every other number on this line.
  const matching = new Set(
    filtered.map((m) => `${m.provider}:${m.provider === 'bedrock' ? parseBedrockId(m.model_id).unscoped : m.model_id}`),
  ).size
  const filterLabel: Record<Filter, string> = { all: 'All', enabled: 'Enabled', disabled: 'Disabled' }
  const filterCount: Record<Filter, number | null> = { all: null, enabled: logical.enabled, disabled: logical.disabled }

  return (
    <>
      {policyRestricted && (
        <p className={styles.policyNote}>
          <LockGlyph />
          <span>
            The organisation's policy limits what is reachable —{' '}
            <Link
              className={styles.policyNoteLink}
              to={{ search: '?tab=policy' }}
              onClick={(e) => {
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

      {loading ? (
        <div className={styles.vendorStrip} aria-hidden="true">
          <Skeleton width="90px" height="18px" borderRadius="var(--radius-full)" />
          <Skeleton width="90px" height="18px" borderRadius="var(--radius-full)" />
        </div>
      ) : (
        <VendorStrip providers={providers} onGoToTab={onGoToTab} />
      )}

      <h2 className={styles.srOnly}>Available models</h2>

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
            placeholder="Search models or IDs"
            value={search}
            onChange={(e) => onSearch(e.target.value)}
            aria-label="Search models or IDs"
          />
          {search && (
            <button type="button" className={styles.searchClear} onClick={() => onSearch('')} aria-label="Clear search">
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
                <line x1="18" y1="6" x2="6" y2="18" />
                <line x1="6" y1="6" x2="18" y2="18" />
              </svg>
            </button>
          )}
        </div>
        <div className={styles.filters} role="group" aria-label="Show">
          {(['all', 'enabled', 'disabled'] as const).map((f) => (
            <button
              key={f}
              type="button"
              aria-pressed={filter === f}
              className={`${styles.chip} ${filter === f ? styles.chipActive : ''}`}
              disabled={loading}
              onClick={() => onFilter(f)}
            >
              {filterLabel[f]}
              {!loading && filterCount[f] !== null && <span className={styles.chipCount}>{filterCount[f]}</span>}
            </button>
          ))}
        </div>
        {!loading && logical.retiredOff > 0 && (
          <label className={styles.showRetired}>
            <input type="checkbox" checked={showRetired} onChange={(e) => onShowRetired(e.target.checked)} />
            Show retired
          </label>
        )}
      </div>

      {!loading && models.length > 0 && (
        <div className={styles.summary}>
          <span className={styles.summaryText}>
            {logical.enabled} of {logical.total} enabled
          </span>
          {logical.optIn > 0 && (
            <>
              <span className={styles.summarySep}>·</span>
              <span className={optInLive ? styles.summaryFlag : styles.summaryText}>
                {optInLive && <OptInGlyph />}
                {awsOptInCountText(logical.optIn)}
              </span>
            </>
          )}
          {logical.retiredOff > 0 && (
            <>
              <span className={styles.summarySep}>·</span>
              <span className={styles.summaryText}>
                {logical.retiredOff} retired{showRetired ? '' : ', hidden'}
                {!showRetired && (
                  <>
                    {' '}
                    <button type="button" className={styles.summaryInline} onClick={() => onShowRetired(true)}>
                      Show
                    </button>
                  </>
                )}
              </span>
            </>
          )}
          {egressEnabled > 0 && (
            <>
              <span className={styles.summarySep}>·</span>
              <span className={styles.summaryFlag}>
                <EgressGlyph />
                {egressEnabled} {egressEnabled === 1 ? 'sends prompts' : 'send prompts'} outside your cloud account
              </span>
            </>
          )}
          {logical.unreachable > 0 && (
            <>
              <span className={styles.summarySep}>·</span>
              <span className={styles.summaryFlag}>
                <UnreachableGlyph />
                {logical.unreachable} not reachable right now
              </span>
            </>
          )}
          {search && (
            <>
              <span className={styles.summarySep}>·</span>
              <span className={styles.summaryText}>
                {matching} matching "{search}"
              </span>
            </>
          )}
        </div>
      )}

      {loading ? (
        <LoadingSkeleton />
      ) : models.length === 0 ? (
        <EmptyCard
          glyph={<CatalogueGlyph />}
          title="No models discovered yet"
          hint="Models appear here when the gateway finds them on Bedrock, on your self-hosted backends, or through a vendor key."
          actions={
            <>
              <Button size="sm" variant="secondary" onClick={onCheckAgain}>
                Check again
              </Button>
              <Button size="sm" variant="ghost" onClick={() => onGoToTab('vendors')}>
                Set up a vendor key
              </Button>
            </>
          }
        />
      ) : filtered.length === 0 && search.trim() ? (
        <EmptyCard
          glyph={<SearchGlyph />}
          title={`No models match "${search.trim()}"`}
          hint={`Search covers names and model IDs.${retiredMatching > 0 ? ` ${retiredMatching} retired ${retiredMatching === 1 ? 'model matches and is' : 'models match and are'} hidden.` : ''}`}
          actions={
            <>
              <Button size="sm" variant="secondary" onClick={() => onSearch('')}>
                Clear search
              </Button>
              {retiredMatching > 0 && (
                <Button size="sm" variant="ghost" onClick={() => onShowRetired(true)}>
                  Show retired
                </Button>
              )}
            </>
          }
        />
      ) : filtered.length === 0 && filter === 'enabled' ? (
        <EmptyCard
          glyph={<CatalogueGlyph />}
          title="No models are enabled yet"
          hint="Turn one on and it becomes available in chat for everyone your policy allows."
          actions={
            <Button size="sm" variant="secondary" onClick={() => onFilter('all')}>
              Show all models
            </Button>
          }
        />
      ) : filtered.length === 0 ? (
        <EmptyCard
          glyph={<CatalogueGlyph />}
          title="Every model is enabled"
          actions={
            <Button size="sm" variant="ghost" onClick={() => onFilter('all')}>
              Show all models
            </Button>
          }
        />
      ) : (
        providerGroups.map(([provider, providerModels]) => (
          <ProviderSection
            key={provider}
            provider={provider}
            models={providerModels}
            allModels={visibleByProvider.get(provider) ?? providerModels}
            togglingIds={togglingIds}
            residency={residency}
            allowedModels={allowedModels}
            firstRun={isVendorProvider(provider) && firstRunVendors.has(provider)}
            searching={searching}
            onReviewAndEnable={onReviewAndEnable}
            onToggle={onToggle}
            onRename={onRename}
          />
        ))
      )}
    </>
  )
}
