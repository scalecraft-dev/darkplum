import { useEffect, useId, useMemo, useRef, useState } from 'react'
import type {
  AdminModel,
  AliasClassifierKind,
  AliasKind,
  AliasRouteTest,
  AliasTargetHealth,
  ModelAlias,
  ModelAliasInput,
  ModelAliasRoute,
  Residency,
} from '../types'
import { ApiError, testModelAlias } from '../lib/api'
import {
  ALIAS_HINT,
  BUILTIN_KEYWORDS,
  MAX_KEYWORDS_PER_ROUTE,
  MAX_KEYWORD_LENGTH,
  MAX_ROUTE_DESCRIPTION,
  ROUTE_NAME_HINT,
  UNREACHABLE_HINT,
  aliasKindOf,
  backendClassOf,
  isReachable,
  isRoutedAlias,
  routerReasonText,
  validateAlias,
  validateRouteName,
} from '../lib/modelRouting'
import { useToast } from '../components/Toast'
import { Badge, Button, Input, Modal, Table, TableEmpty } from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './AliasesSection.module.css'

interface Props {
  aliases: ModelAlias[]
  models: AdminModel[]
  loading: boolean
  error: string | null
  residency: Residency
  onRetry: () => void
  onSave: (alias: string, body: ModelAliasInput) => Promise<void>
  onDelete: (alias: string) => Promise<void>
}

type HealthKind = 'ok' | 'missing' | 'disabled' | 'blocked' | 'unreachable'

/** Most permanent problem first: a missing target outranks a disabled one,
 *  a policy refusal outranks a backend that is merely away for a few minutes. */
function healthOf(t: AliasTargetHealth, residency: Residency): HealthKind {
  if (!t.target_exists) return 'missing'
  if (!t.target_enabled) return 'disabled'
  if (residency === 'self_hosted_only' && t.target_backend_class === 'cloud_managed') {
    return 'blocked'
  }
  if (t.target_available === false) return 'unreachable'
  return 'ok'
}

const HEALTH_COPY: Record<HealthKind, { label: string; variant: 'success' | 'warning' | 'danger'; hint: string }> = {
  ok: {
    label: 'Active',
    variant: 'success',
    hint: '',
  },
  missing: {
    label: 'Target missing',
    variant: 'danger',
    hint: 'No model has this ID. Requests using this alias are refused.',
  },
  disabled: {
    label: 'Target disabled',
    variant: 'warning',
    hint: 'Enable the target model, or point this alias at one that is on.',
  },
  blocked: {
    label: 'Blocked by policy',
    variant: 'warning',
    hint: 'The routing policy allows self-hosted models only, and this target is cloud-managed.',
  },
  unreachable: {
    label: 'Target unreachable',
    variant: 'warning',
    hint: 'The target model is not reachable right now. The alias stays pointed at it and works again when its backend is back.',
  },
}

/** Short words for one route's problem, next to the route it belongs to. */
const ROUTE_HEALTH_LABEL: Record<Exclude<HealthKind, 'ok'>, string> = {
  missing: 'target missing',
  disabled: 'target disabled',
  blocked: 'blocked by policy',
  unreachable: 'target unreachable',
}

const HEALTH_RANK: Record<HealthKind, number> = {
  ok: 0,
  unreachable: 1,
  blocked: 2,
  disabled: 3,
  missing: 4,
}

/** The state of the whole alias is the state of its worst route: one broken
 *  route is a refusal for every message that classifies into it. */
function aliasHealth(a: ModelAlias, residency: Residency): HealthKind {
  if (!isRoutedAlias(a)) return healthOf(a, residency)
  const routes = a.routes ?? []
  if (routes.length === 0) return 'missing'
  return routes
    .map((r) => healthOf(r, residency))
    .reduce((worst, next) => (HEALTH_RANK[next] > HEALTH_RANK[worst] ? next : worst), 'ok')
}

const CLASSIFIER_LABEL: Record<AliasClassifierKind, string> = {
  rules: 'keyword rules',
  semantic_router: 'semantic router',
  model: 'model classifier',
}

// ---------------------------------------------------------------------------
// Delete — two-step confirm, matching the pattern used for users.
// ---------------------------------------------------------------------------

function DeleteAliasButton({ alias, onDelete }: { alias: string; onDelete: (alias: string) => Promise<void> }) {
  const [confirming, setConfirming] = useState(false)
  const [loading, setLoading] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => () => { if (timer.current) clearTimeout(timer.current) }, [])

  const handleClick = async () => {
    if (!confirming) {
      setConfirming(true)
      timer.current = setTimeout(() => setConfirming(false), 3000)
      return
    }
    if (timer.current) clearTimeout(timer.current)
    setLoading(true)
    try {
      await onDelete(alias)
    } finally {
      setLoading(false)
      setConfirming(false)
    }
  }

  return (
    <button
      type="button"
      className={`${styles.rowBtn} ${confirming ? styles.rowBtnConfirm : ''}`}
      onClick={() => void handleClick()}
      disabled={loading}
      aria-label={confirming ? `Confirm deleting alias ${alias}` : `Delete alias ${alias}`}
    >
      {loading ? 'Deleting...' : confirming ? 'Confirm?' : 'Delete'}
    </button>
  )
}

// ---------------------------------------------------------------------------
// Shared bits of the editor
// ---------------------------------------------------------------------------

/** A <option> cannot hold a badge, so the caveats ride in the text. */
function targetOptionLabel(m: AdminModel): string {
  const notes = [
    m.enabled ? '' : 'disabled',
    isReachable(m) ? '' : 'not reachable',
  ].filter(Boolean)
  return notes.length > 0 ? `${m.model_id} (${notes.join(', ')})` : m.model_id
}

function ModelOptions({ models }: { models: AdminModel[] }) {
  const grouped = useMemo(() => {
    const sorted = [...models].sort((a, b) => a.model_id.localeCompare(b.model_id))
    return {
      selfHosted: sorted.filter((m) => backendClassOf(m) === 'self_hosted'),
      cloud: sorted.filter((m) => backendClassOf(m) === 'cloud_managed'),
    }
  }, [models])

  return (
    <>
      {grouped.selfHosted.length > 0 && (
        <optgroup label="Self-hosted">
          {grouped.selfHosted.map((m) => (
            <option key={m.id} value={m.model_id}>{targetOptionLabel(m)}</option>
          ))}
        </optgroup>
      )}
      {grouped.cloud.length > 0 && (
        <optgroup label="Cloud-managed">
          {grouped.cloud.map((m) => (
            <option key={m.id} value={m.model_id}>{targetOptionLabel(m)}</option>
          ))}
        </optgroup>
      )}
    </>
  )
}

/** What is wrong with this target as things stand, in one sentence, or null. */
function targetWarning(
  models: AdminModel[],
  targetModelId: string,
  residency: Residency,
): string | null {
  if (!targetModelId) return null
  const model = models.find((m) => m.model_id === targetModelId)
  if (!model) return 'No model has this ID. Requests that take this route are refused.'
  if (!model.enabled) return 'This model is turned off, so this route is refused until you enable it.'
  if (residency === 'self_hosted_only' && backendClassOf(model) === 'cloud_managed') {
    return 'The routing policy allows self-hosted models only, so this route is refused until the policy changes.'
  }
  if (!isReachable(model)) return UNREACHABLE_HINT
  return null
}

// ---------------------------------------------------------------------------
// Keywords
// ---------------------------------------------------------------------------

interface KeywordsProps {
  routeName: string
  keywords: string[]
  onChange: (keywords: string[]) => void
}

/** Literal text an administrator wants to send down this route. Case is folded
 *  on the way in, because the gateway matches case-folded substrings. */
function KeywordEditor({ routeName, keywords, onChange }: KeywordsProps) {
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')
  const inputId = useId()
  const label = routeName || 'this route'

  const add = (raw: string) => {
    const value = raw.trim().toLowerCase()
    if (!value) return
    if (value.length > MAX_KEYWORD_LENGTH) {
      setError(`Keywords are ${MAX_KEYWORD_LENGTH} characters or fewer.`)
      return
    }
    if (keywords.includes(value)) {
      setError('That keyword is already on this route.')
      return
    }
    if (keywords.length >= MAX_KEYWORDS_PER_ROUTE) {
      setError(`A route holds ${MAX_KEYWORDS_PER_ROUTE} keywords at most.`)
      return
    }
    setError('')
    setDraft('')
    onChange([...keywords, value])
  }

  const remove = (value: string) => {
    setError('')
    onChange(keywords.filter((k) => k !== value))
  }

  const unusedBuiltins = BUILTIN_KEYWORDS.filter((b) => !keywords.includes(b.token))

  return (
    <div className={styles.keywords}>
      <label className={styles.fieldLabel} htmlFor={inputId}>Keywords</label>
      <p className={styles.fieldHint}>
        Plain text to look for in the message, matched anywhere in it and ignoring case.
        The route highest in the list wins.
      </p>

      {keywords.length > 0 && (
        <ul className={styles.chipList}>
          {keywords.map((k) => (
            <li key={k} className={styles.chip}>
              <code className={styles.chipText}>{k}</code>
              <button
                type="button"
                className={styles.chipRemove}
                onClick={() => remove(k)}
                aria-label={`Remove keyword ${k} from ${label}`}
              >
                <svg width="10" height="10" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                  <line x1="3" y1="3" x2="11" y2="11" />
                  <line x1="11" y1="3" x2="3" y2="11" />
                </svg>
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className={styles.keywordInputRow}>
        <input
          id={inputId}
          type="text"
          className={styles.keywordInput}
          value={draft}
          spellCheck={false}
          autoComplete="off"
          placeholder="invoice"
          aria-describedby={error ? `${inputId}-error` : undefined}
          onChange={(e) => { setDraft(e.target.value); setError('') }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ',') {
              e.preventDefault()
              add(draft)
            } else if (e.key === 'Backspace' && draft === '' && keywords.length > 0) {
              remove(keywords[keywords.length - 1])
            }
          }}
        />
        <button
          type="button"
          className={styles.rowBtn}
          onClick={() => add(draft)}
          disabled={draft.trim() === ''}
        >
          Add keyword
        </button>
      </div>
      {error && <span className={styles.fieldError} id={`${inputId}-error`} role="alert">{error}</span>}

      {unusedBuiltins.length > 0 && (
        <div className={styles.builtins}>
          <span className={styles.builtinsLead}>Signals the gateway can detect on its own:</span>
          <ul className={styles.builtinList}>
            {unusedBuiltins.map((b) => (
              <li key={b.token} className={styles.builtinItem}>
                <button
                  type="button"
                  className={styles.builtinBtn}
                  onClick={() => add(b.token)}
                  aria-label={`Add ${b.token} to ${label}`}
                >
                  {b.token}
                </button>
                <span className={styles.builtinText}>{b.explanation}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Route rows
// ---------------------------------------------------------------------------

interface DraftRoute {
  /** Stable across renames and reordering, so React keeps the right input. */
  key: string
  route_name: string
  description: string
  target_model_id: string
  keywords: string[]
}

interface RouteRowProps {
  route: DraftRoute
  index: number
  count: number
  isDefault: boolean
  radioName: string
  models: AdminModel[]
  residency: Residency
  keywordsUsed: boolean
  nameError: string
  descriptionError: string
  targetError: string
  onChange: (patch: Partial<DraftRoute>) => void
  onMakeDefault: () => void
  onMove: (direction: -1 | 1) => void
  onRemove: () => void
}

function RouteRow({
  route,
  index,
  count,
  isDefault,
  radioName,
  models,
  residency,
  keywordsUsed,
  nameError,
  descriptionError,
  targetError,
  onChange,
  onMakeDefault,
  onMove,
  onRemove,
}: RouteRowProps) {
  const fieldId = useId()
  const label = route.route_name || `route ${index + 1}`
  const warning = targetWarning(models, route.target_model_id, residency)

  return (
    <li className={styles.route}>
      <div className={styles.routeBar}>
        <span className={styles.routePosition} aria-hidden="true">{index + 1}</span>
        <label className={styles.defaultOption}>
          <input
            type="radio"
            name={radioName}
            checked={isDefault}
            onChange={onMakeDefault}
            aria-label={`Make ${label} the default route`}
          />
          Default route
        </label>
        <div className={styles.routeBarActions}>
          <button
            type="button"
            className={styles.iconBtn}
            onClick={() => onMove(-1)}
            disabled={index === 0}
            aria-label={`Move ${label} up`}
          >
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <polyline points="18 15 12 9 6 15" />
            </svg>
          </button>
          <button
            type="button"
            className={styles.iconBtn}
            onClick={() => onMove(1)}
            disabled={index === count - 1}
            aria-label={`Move ${label} down`}
          >
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <polyline points="6 9 12 15 18 9" />
            </svg>
          </button>
          <button
            type="button"
            className={styles.rowBtn}
            onClick={onRemove}
            disabled={count === 1}
            aria-label={`Remove ${label}`}
            title={count === 1 ? 'A routed alias needs at least one route' : undefined}
          >
            Remove
          </button>
        </div>
      </div>

      <div className={styles.routeGrid}>
        <Input
          label="Route name"
          id={`${fieldId}-name`}
          value={route.route_name}
          onChange={(e) => onChange({ route_name: e.target.value })}
          error={nameError}
          placeholder="code"
          spellCheck={false}
          autoComplete="off"
          className={styles.monoInput}
        />
        <Input
          label="What it is for"
          id={`${fieldId}-desc`}
          value={route.description}
          onChange={(e) => onChange({ description: e.target.value.slice(0, MAX_ROUTE_DESCRIPTION) })}
          error={descriptionError}
          placeholder="Code, diffs and stack traces"
          autoComplete="off"
        />
        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor={`${fieldId}-target`}>Sends to</label>
          <select
            id={`${fieldId}-target`}
            className={styles.select}
            value={route.target_model_id}
            onChange={(e) => onChange({ target_model_id: e.target.value })}
          >
            <option value="">Choose a model</option>
            <ModelOptions models={models} />
          </select>
          {targetError && <span className={styles.fieldError}>{targetError}</span>}
        </div>
      </div>

      {warning && <p className={styles.formWarning}>{warning}</p>}

      {keywordsUsed ? (
        <KeywordEditor
          routeName={route.route_name}
          keywords={route.keywords}
          onChange={(keywords) => onChange({ keywords })}
        />
      ) : (
        <p className={styles.fieldHint}>
          The classifier reads the route name and description instead of keywords.
          {route.keywords.length > 0 &&
            ` The ${route.keywords.length} keyword${route.keywords.length === 1 ? '' : 's'} on this route are kept, unused, until you go back to keyword rules.`}
        </p>
      )}
    </li>
  )
}

// ---------------------------------------------------------------------------
// Editor
// ---------------------------------------------------------------------------

interface EditorProps {
  editing: ModelAlias | null
  models: AdminModel[]
  residency: Residency
  existing: ModelAlias[]
  onClose: () => void
  onSave: (alias: string, body: ModelAliasInput) => Promise<void>
}

let routeKeySeq = 0
function newRouteKey(): string {
  routeKeySeq += 1
  return `route-${routeKeySeq}`
}

function toDraftRoutes(routes: ModelAliasRoute[]): DraftRoute[] {
  return routes.map((r) => ({
    key: newRouteKey(),
    route_name: r.route_name,
    description: r.description,
    target_model_id: r.target_model_id,
    keywords: [...(r.keywords ?? [])],
  }))
}

function blankRoute(): DraftRoute {
  return { key: newRouteKey(), route_name: '', description: '', target_model_id: '', keywords: [] }
}

interface RouteErrors {
  name: string
  description: string
  target: string
}

function AliasEditor({ editing, models, residency, existing, onClose, onSave }: EditorProps) {
  const formId = useId()
  // Built once, so the keys the default route is tracked by survive renames.
  const initial = useRef<{ routes: DraftRoute[]; defaultKey: string } | null>(null)
  if (!initial.current) {
    const drafts =
      editing?.routes && editing.routes.length > 0 ? toDraftRoutes(editing.routes) : [blankRoute()]
    initial.current = {
      routes: drafts,
      defaultKey:
        drafts.find((r) => r.route_name === editing?.default_route)?.key ?? drafts[0].key,
    }
  }
  const [alias, setAlias] = useState(editing?.alias ?? '')
  const [kind, setKind] = useState<AliasKind>(editing ? aliasKindOf(editing) : 'static')
  const [target, setTarget] = useState(editing?.target_model_id ?? '')
  const [routes, setRoutes] = useState<DraftRoute[]>(initial.current.routes)
  /** Which route is the default, by key rather than by name: renaming the
   *  default route must not quietly hand the job to another one. */
  const [defaultKey, setDefaultKey] = useState(initial.current.defaultKey)
  const [classifierKind, setClassifierKind] = useState<AliasClassifierKind>(
    editing?.classifier?.kind ?? 'rules',
  )
  const [classifierModel, setClassifierModel] = useState(
    editing?.classifier?.kind === 'model' ? editing.classifier.model_id : '',
  )
  const [aliasError, setAliasError] = useState('')
  const [targetError, setTargetError] = useState('')
  const [routeErrors, setRouteErrors] = useState<Record<string, RouteErrors>>({})
  const [formError, setFormError] = useState('')
  const [announcement, setAnnouncement] = useState('')
  const [saving, setSaving] = useState(false)

  const selfHosted = useMemo(
    () =>
      [...models]
        .filter((m) => backendClassOf(m) === 'self_hosted')
        .sort((a, b) => a.model_id.localeCompare(b.model_id)),
    [models],
  )

  /** A routed alias must name a default the API can find, so removing the
   *  default route hands the job to the first one rather than to nothing. */
  useEffect(() => {
    if (routes.length === 0) return
    if (routes.some((r) => r.key === defaultKey)) return
    setDefaultKey(routes[0].key)
  }, [routes, defaultKey])

  const patchRoute = (key: string, patch: Partial<DraftRoute>) => {
    setRoutes((prev) => prev.map((r) => (r.key === key ? { ...r, ...patch } : r)))
    setRouteErrors((prev) => (prev[key] ? { ...prev, [key]: { name: '', description: '', target: '' } } : prev))
    setFormError('')
  }

  const moveRoute = (index: number, direction: -1 | 1) => {
    const next = index + direction
    if (next < 0 || next >= routes.length) return
    setRoutes((prev) => {
      const copy = [...prev]
      const [moved] = copy.splice(index, 1)
      copy.splice(next, 0, moved)
      return copy
    })
    const name = routes[index].route_name || `Route ${index + 1}`
    setAnnouncement(`${name} moved to position ${next + 1} of ${routes.length}`)
  }

  const validate = (): ModelAliasInput | null => {
    const trimmedAlias = alias.trim()
    const aliasProblem = validateAlias(trimmedAlias)
    if (aliasProblem) {
      setAliasError(aliasProblem)
      return null
    }
    if (!editing && existing.some((a) => a.alias === trimmedAlias)) {
      setAliasError('This alias already exists. Edit it instead.')
      return null
    }
    setAliasError('')

    if (kind === 'static') {
      if (!target) {
        setTargetError('Choose the model this alias points at.')
        return null
      }
      setTargetError('')
      return { target_model_id: target }
    }

    const errors: Record<string, RouteErrors> = {}
    const seen = new Set<string>()
    for (const r of routes) {
      const name = r.route_name.trim()
      const rowErrors: RouteErrors = { name: '', description: '', target: '' }
      const nameProblem = validateRouteName(name)
      if (nameProblem) rowErrors.name = nameProblem
      else if (seen.has(name)) rowErrors.name = 'Two routes cannot share a name.'
      else seen.add(name)
      if (!r.description.trim()) {
        rowErrors.description = 'Say what this route is for — the classifier reads it.'
      }
      if (!r.target_model_id) rowErrors.target = 'Choose the model this route sends to.'
      if (rowErrors.name || rowErrors.description || rowErrors.target) errors[r.key] = rowErrors
    }
    if (Object.keys(errors).length > 0) {
      setRouteErrors(errors)
      setFormError('Every route needs a name, a description and a model.')
      return null
    }
    setRouteErrors({})

    const chosenDefault = (
      routes.find((r) => r.key === defaultKey) ?? routes[0]
    )?.route_name.trim()
    if (!chosenDefault) {
      setFormError('Choose which route answers when nothing else matches.')
      return null
    }
    if (classifierKind === 'model' && !classifierModel) {
      setFormError('Choose the self-hosted model that picks the route.')
      return null
    }
    setFormError('')

    return {
      kind: 'routed',
      default_route: chosenDefault,
      classifier:
        classifierKind === 'model'
          ? { kind: 'model', model_id: classifierModel }
          : { kind: classifierKind },
      routes: routes.map((r) => ({
        route_name: r.route_name.trim(),
        description: r.description.trim(),
        target_model_id: r.target_model_id,
        keywords: r.keywords,
      })),
    }
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const body = validate()
    if (!body) return
    setSaving(true)
    try {
      await onSave(alias.trim(), body)
      onClose()
    } catch (err) {
      // The API's own words, against the field they belong to where that is
      // knowable. A routed body can fail for a reason no single field owns.
      const status = err instanceof ApiError ? err.status : 0
      const message = (err as Error).message
      if (status === 409 && kind === 'static') setAliasError(message)
      else if (status === 404 && kind === 'static') setTargetError(message)
      else if (status === 400 && kind === 'static' && /alias/i.test(message)) setAliasError(message)
      else if (kind === 'static') setTargetError(message)
      else setFormError(message)
    } finally {
      setSaving(false)
    }
  }

  const staticWarning = targetWarning(models, target, residency)
  const keywordsUsed = classifierKind === 'rules'

  return (
    <form className={styles.editor} onSubmit={(e) => void handleSubmit(e)}>
      <div className={styles.editorHead}>
        <h3 className={styles.editorTitle}>
          {editing ? `Edit ${editing.alias}` : 'New alias'}
        </h3>
        <p className={styles.editorSubtitle}>
          {editing
            ? 'Anyone already using this alias follows the change on their next message.'
            : 'A stable name people pick in chat. You decide what it reaches; they are told which model answered.'}
        </p>
      </div>

      {formError && <p className={styles.serverError} role="alert">{formError}</p>}

      <div className={styles.editorTop}>
        <div className={styles.field}>
          <Input
            label="Alias"
            type="text"
            value={alias}
            onChange={(e) => { setAlias(e.target.value); setAliasError('') }}
            placeholder="fast"
            error={aliasError}
            disabled={Boolean(editing)}
            autoFocus={!editing}
            spellCheck={false}
            autoComplete="off"
            className={styles.monoInput}
          />
          {!editing && <p className={styles.fieldHint}>{ALIAS_HINT}</p>}
        </div>

        <fieldset className={styles.choiceSet}>
          <legend className={styles.fieldLabel}>What it reaches</legend>
          <div className={styles.choices}>
            <label className={`${styles.choice} ${kind === 'static' ? styles.choiceChecked : ''}`}>
              <span className={styles.choiceTop}>
                <input
                  type="radio"
                  name={`${formId}-kind`}
                  checked={kind === 'static'}
                  onChange={() => { setKind('static'); setFormError('') }}
                />
                <span className={styles.choiceLabel}>One model</span>
              </span>
              <span className={styles.choiceText}>
                Every message on this alias goes to the same model.
              </span>
            </label>
            <label className={`${styles.choice} ${kind === 'routed' ? styles.choiceChecked : ''}`}>
              <span className={styles.choiceTop}>
                <input
                  type="radio"
                  name={`${formId}-kind`}
                  checked={kind === 'routed'}
                  onChange={() => { setKind('routed'); setFormError('') }}
                />
                <span className={styles.choiceLabel}>Routes you write</span>
              </span>
              <span className={styles.choiceText}>
                You write the routes; a classifier picks one of them and nothing else.
              </span>
            </label>
          </div>
        </fieldset>
      </div>

      {kind === 'static' ? (
        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor={`${formId}-target`}>Points at</label>
          <select
            id={`${formId}-target`}
            className={styles.select}
            value={target}
            onChange={(e) => { setTarget(e.target.value); setTargetError('') }}
          >
            <option value="">Choose a model</option>
            <ModelOptions models={models} />
          </select>
          {targetError && <span className={styles.fieldError}>{targetError}</span>}
          {staticWarning && <p className={styles.formWarning}>{staticWarning}</p>}
        </div>
      ) : (
        <>
          <div className={styles.routesHead}>
            <div>
              <h4 className={styles.subheading}>Routes</h4>
              <p className={styles.editorSubtitle}>
                In order. Every message takes exactly one of these; the default answers when
                nothing else matches. {ROUTE_NAME_HINT}
              </p>
            </div>
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={() => setRoutes((prev) => [...prev, blankRoute()])}
            >
              Add route
            </Button>
          </div>

          <ol className={styles.routeList}>
            {routes.map((r, i) => (
              <RouteRow
                key={r.key}
                route={r}
                index={i}
                count={routes.length}
                isDefault={defaultKey === r.key}
                radioName={`${formId}-default`}
                models={models}
                residency={residency}
                keywordsUsed={keywordsUsed}
                nameError={routeErrors[r.key]?.name ?? ''}
                descriptionError={routeErrors[r.key]?.description ?? ''}
                targetError={routeErrors[r.key]?.target ?? ''}
                onChange={(patch) => patchRoute(r.key, patch)}
                onMakeDefault={() => setDefaultKey(r.key)}
                onMove={(direction) => moveRoute(i, direction)}
                onRemove={() => setRoutes((prev) => prev.filter((x) => x.key !== r.key))}
              />
            ))}
          </ol>
          <p className={styles.srOnly} role="status" aria-live="polite">{announcement}</p>

          <fieldset className={styles.choiceSet}>
            <legend className={styles.fieldLabel}>How the route is chosen</legend>
            <div className={styles.choices}>
              <label className={`${styles.choice} ${classifierKind === 'rules' ? styles.choiceChecked : ''}`}>
                <span className={styles.choiceTop}>
                  <input
                    type="radio"
                    name={`${formId}-classifier`}
                    checked={classifierKind === 'rules'}
                    onChange={() => { setClassifierKind('rules'); setFormError('') }}
                  />
                  <span className={styles.choiceLabel}>Keyword rules</span>
                </span>
                <span className={styles.choiceText}>
                  Your keywords decide, top route first. No model reads the message to sort it.
                </span>
              </label>
              <label className={`${styles.choice} ${classifierKind === 'semantic_router' ? styles.choiceChecked : ''}`}>
                <span className={styles.choiceTop}>
                  <input
                    type="radio"
                    name={`${formId}-classifier`}
                    checked={classifierKind === 'semantic_router'}
                    onChange={() => { setClassifierKind('semantic_router'); setFormError('') }}
                  />
                  <span className={styles.choiceLabel}>Semantic Router</span>
                </span>
                <span className={styles.choiceText}>
                  An open-source intent classifier (vLLM Semantic Router) running on CPU inside the
                  cluster; route names must match its configured categories.
                </span>
              </label>
              <label className={`${styles.choice} ${classifierKind === 'model' ? styles.choiceChecked : ''}`}>
                <span className={styles.choiceTop}>
                  <input
                    type="radio"
                    name={`${formId}-classifier`}
                    checked={classifierKind === 'model'}
                    onChange={() => { setClassifierKind('model'); setFormError('') }}
                  />
                  <span className={styles.choiceLabel}>A model you host</span>
                </span>
                <span className={styles.choiceText}>
                  A model reads the message and answers with one of your route names.
                </span>
              </label>
            </div>
          </fieldset>

          {classifierKind === 'model' && (
            <div className={styles.field}>
              <label className={styles.fieldLabel} htmlFor={`${formId}-classifier-model`}>
                Classifier model
              </label>
              <select
                id={`${formId}-classifier-model`}
                className={styles.select}
                value={classifierModel}
                onChange={(e) => { setClassifierModel(e.target.value); setFormError('') }}
              >
                <option value="">Choose a self-hosted model</option>
                {selfHosted.map((m) => (
                  <option key={m.id} value={m.model_id}>{targetOptionLabel(m)}</option>
                ))}
              </select>
              <p className={styles.fieldHint}>
                Self-hosted models only: the message is sent to this model before it is routed, so
                it must not leave your infrastructure to be sorted.
              </p>
              {selfHosted.length === 0 && (
                <p className={styles.formWarning}>
                  No self-hosted models have been discovered, so nothing can classify yet. Use
                  keyword rules until one is available.
                </p>
              )}
            </div>
          )}
        </>
      )}

      <div className={styles.editorFooter}>
        <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
        <Button type="submit" disabled={saving}>
          {saving ? 'Saving...' : editing ? 'Save alias' : 'Create alias'}
        </Button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------------------
// Test a prompt
// ---------------------------------------------------------------------------

function TestPromptModal({
  alias,
  open,
  onClose,
}: {
  alias: ModelAlias | null
  open: boolean
  onClose: () => void
}) {
  const [text, setText] = useState('')
  const [result, setResult] = useState<AliasRouteTest | null>(null)
  const [error, setError] = useState('')
  const [running, setRunning] = useState(false)
  const textId = useId()

  useEffect(() => {
    if (!open) return
    setText('')
    setResult(null)
    setError('')
  }, [open, alias])

  const run = async () => {
    if (!alias || !text.trim()) return
    setRunning(true)
    setError('')
    try {
      setResult(await testModelAlias(alias.alias, text))
    } catch (err) {
      setResult(null)
      setError((err as Error).message)
    } finally {
      setRunning(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={alias ? `Test a prompt on ${alias.alias}` : 'Test a prompt'}
      description="Shows which route this message would take. The model it names is not called and answers nothing."
    >
      <div className={styles.testBody}>
        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor={textId}>Message</label>
          <textarea
            id={textId}
            className={styles.textarea}
            rows={5}
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Paste the kind of message you want to check."
            autoFocus
          />
        </div>

        <div className={styles.testFooter}>
          <Button type="button" variant="ghost" onClick={onClose}>Close</Button>
          <Button type="button" onClick={() => void run()} disabled={running || text.trim() === ''}>
            {running ? 'Checking...' : 'Check the route'}
          </Button>
        </div>

        <div aria-live="polite">
          {error && (
            <div className={styles.error} role="alert">
              <p className={styles.errorTitle}>The route could not be checked</p>
              <p className={styles.errorDetail}>{error}</p>
            </div>
          )}

          {result && (
            <div className={styles.testResult}>
              <dl className={styles.testFacts}>
                <div className={styles.testFact}>
                  <dt className={styles.fieldLabel}>Route</dt>
                  <dd><code className={styles.alias}>{result.route_name}</code></dd>
                </div>
                <div className={styles.testFact}>
                  <dt className={styles.fieldLabel}>Sends to</dt>
                  <dd className={styles.idCell}>
                    <code className={styles.targetId}>{result.target_model_id}</code>
                    <CopyButton value={result.target_model_id} label="Copy" />
                  </dd>
                </div>
                <div className={styles.testFact}>
                  <dt className={styles.fieldLabel}>Why</dt>
                  <dd className={styles.testReason} title={result.reason}>
                    {routerReasonText(result.reason)}
                  </dd>
                </div>
                <div className={styles.testFact}>
                  <dt className={styles.fieldLabel}>Took</dt>
                  <dd className={styles.testReason}>{result.latency_ms}ms</dd>
                </div>
              </dl>

              {result.would_be_refused && (
                <div className={styles.refusal} role="alert">
                  <p className={styles.refusalTitle}>
                    This pick would be refused: {result.would_be_refused.message}
                  </p>
                  <p className={styles.refusalHint}>
                    Fix the route or the policy. Nothing is sent to another model instead — the
                    message is refused with the route named.
                  </p>
                  <code className={styles.refusalCode}>{result.would_be_refused.code}</code>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </Modal>
  )
}

// ---------------------------------------------------------------------------
// Section
// ---------------------------------------------------------------------------

export function AliasesSection({
  aliases,
  models,
  loading,
  error,
  residency,
  onRetry,
  onSave,
  onDelete,
}: Props) {
  const { toast } = useToast()
  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<ModelAlias | null>(null)
  const [testing, setTesting] = useState<ModelAlias | null>(null)

  const openCreate = () => { setEditing(null); setEditorOpen(true) }
  const openEdit = (a: ModelAlias) => { setEditing(a); setEditorOpen(true) }

  const handleDelete = async (alias: string) => {
    try {
      await onDelete(alias)
      toast(`Alias ${alias} deleted`, 'success')
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }

  return (
    <section className={styles.card} aria-labelledby="aliases-heading">
      <div className={styles.head}>
        <div>
          <h2 id="aliases-heading" className={styles.title}>Aliases</h2>
          <p className={styles.subtitle}>
            Stable names people pick in chat. An alias points at one model, or carries the routes
            you write and lets a classifier pick among them. Either way the reply says which model
            answered.
          </p>
        </div>
        <Button
          size="sm"
          onClick={openCreate}
          disabled={loading || models.length === 0 || editorOpen}
          title={models.length === 0 ? 'No models have been discovered yet' : undefined}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          Add alias
        </Button>
      </div>

      {editorOpen && (
        <AliasEditor
          /* Remounts per alias so the draft never carries over between them. */
          key={editing?.alias ?? 'new'}
          editing={editing}
          models={models}
          residency={residency}
          existing={aliases}
          onClose={() => setEditorOpen(false)}
          onSave={onSave}
        />
      )}

      {error ? (
        <div className={styles.error} role="alert">
          <p className={styles.errorTitle}>Aliases could not be loaded</p>
          <p className={styles.errorDetail}>{error}</p>
          <Button variant="secondary" size="sm" onClick={onRetry}>Try again</Button>
        </div>
      ) : (
        <div className={styles.tableWrap}>
          <Table>
            <thead>
              <tr>
                <th>Alias</th>
                <th>Reaches</th>
                <th>Status</th>
                <th className={styles.actionsCol}></th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                Array.from({ length: 2 }).map((_, i) => (
                  <tr key={i}>
                    <td><Skeleton width="90px" height="14px" /></td>
                    <td><Skeleton width="200px" height="14px" /></td>
                    <td><Skeleton width="60px" height="18px" borderRadius="var(--radius-full)" /></td>
                    <td className={styles.actionsCol}><Skeleton width="60px" height="20px" /></td>
                  </tr>
                ))
              ) : aliases.length === 0 ? (
                <TableEmpty colSpan={4}>
                  <div className={styles.empty}>
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
                      <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
                    </svg>
                    <span className={styles.emptyTitle}>No aliases yet</span>
                    <span className={styles.emptyHint}>
                      Give a model a short name such as <code className={styles.emptyCode}>fast</code>{' '}
                      and swap the model behind it whenever you like.
                    </span>
                  </div>
                </TableEmpty>
              ) : (
                aliases.map((a) => {
                  const routed = isRoutedAlias(a)
                  const health = aliasHealth(a, residency)
                  const copy = HEALTH_COPY[health]
                  const routes = a.routes ?? []
                  const flaggedRoutes = routes
                    .map((r) => ({ route: r, health: healthOf(r, residency) }))
                    .filter((r) => r.health !== 'ok')
                  return (
                    <tr key={a.alias} className={health === 'ok' ? '' : styles.rowFlagged}>
                      <td>
                        <div className={styles.aliasCell}>
                          <div className={styles.idCell}>
                            <code className={styles.alias}>{a.alias}</code>
                            <CopyButton value={a.alias} label="Copy" />
                          </div>
                          <Badge variant={routed ? 'info' : 'neutral'} bordered>
                            {routed ? 'Routed' : 'Static'}
                          </Badge>
                        </div>
                      </td>
                      <td>
                        {routed ? (
                          <div className={styles.reachCell}>
                            <span className={styles.routeSummary}>
                              {routes.length} {routes.length === 1 ? 'route' : 'routes'} · default{' '}
                              <code className={styles.targetId}>{a.default_route ?? '—'}</code> ·{' '}
                              {CLASSIFIER_LABEL[a.classifier?.kind ?? 'rules']}
                            </span>
                            {flaggedRoutes.length > 0 && (
                              <ul className={styles.routeFlags}>
                                {flaggedRoutes.map(({ route, health: routeHealth }) => (
                                  <li key={route.route_name}>
                                    <code className={styles.targetId}>{route.route_name}</code>
                                    {' → '}
                                    <code className={styles.targetId}>{route.target_model_id}</code>
                                    {` — ${ROUTE_HEALTH_LABEL[routeHealth as Exclude<HealthKind, 'ok'>]}`}
                                  </li>
                                ))}
                              </ul>
                            )}
                          </div>
                        ) : (
                          <div className={styles.idCell}>
                            <code className={styles.targetId}>{a.target_model_id || '—'}</code>
                            {a.target_model_id && <CopyButton value={a.target_model_id} label="Copy" />}
                          </div>
                        )}
                      </td>
                      <td>
                        <Badge variant={copy.variant} bordered={health !== 'ok'}>
                          {copy.label}
                        </Badge>
                        {copy.hint && <p className={styles.statusHint}>{copy.hint}</p>}
                      </td>
                      <td className={styles.actionsCol}>
                        <div className={styles.rowActions}>
                          {routed && (
                            <button
                              type="button"
                              className={styles.rowBtn}
                              onClick={() => setTesting(a)}
                              aria-label={`Test a prompt on alias ${a.alias}`}
                            >
                              Test a prompt
                            </button>
                          )}
                          <button
                            type="button"
                            className={styles.rowBtn}
                            onClick={() => openEdit(a)}
                            aria-label={`Edit alias ${a.alias}`}
                          >
                            Edit
                          </button>
                          <DeleteAliasButton alias={a.alias} onDelete={handleDelete} />
                        </div>
                      </td>
                    </tr>
                  )
                })
              )}
            </tbody>
          </Table>
        </div>
      )}

      <TestPromptModal
        alias={testing}
        open={testing !== null}
        onClose={() => setTesting(null)}
      />
    </section>
  )
}
