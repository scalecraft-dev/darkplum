import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import * as VisuallyHidden from '@radix-ui/react-visually-hidden'
import {
  deleteKeyPolicy,
  deleteUserPolicy,
  getKeyPolicyEffective,
  getUserPolicyEffective,
  previewKeyPolicy,
  previewUserPolicy,
  putKeyPolicy,
  putUserPolicy,
} from '../lib/api'
import {
  INHERIT_ALL,
  POLICY_RULE,
  POLICY_TIMING,
  PRINCIPAL_RESIDENCIES,
  aliasTargetIds,
  isInheritAll,
  modelLabel,
  residencyLabel,
  samePolicy,
  sourceChip,
} from '../lib/policy'
import { residencyRank } from '../lib/policy'
import type {
  AdminModel,
  ModelAlias,
  PolicyPreview,
  PolicySource,
  PreviewEffectivePolicy,
  PrincipalPolicy,
  PrincipalResidency,
  Residency,
} from '../types'
import { useToast } from '../components/Toast'
import { Button } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ModelAllowlistPicker } from './ModelAllowlistPicker'
import { ResidencyLadder, type LadderRung } from './ResidencyLadder'
import styles from './PrincipalPolicyDrawer.module.css'

/** Who is being edited. The drawer is identical for both; only the endpoints
 *  and a few sentences differ. */
export interface PolicyPrincipalRef {
  type: 'user' | 'api_key'
  id: string
  /** The name on the row that opened this — an email, or a key name. */
  title: string
  /** What sits under it: a full name, or the key's owner and prefix. */
  subtitle: string
}

interface Props {
  principal: PolicyPrincipalRef | null
  /** Every discovered model. The allowlist offers the enabled ones. */
  models: AdminModel[]
  /** Aliases, for the "add every target of this alias" helper. */
  aliases: ModelAlias[]
  onClose: () => void
  /** A row was written or reset, so the lists behind the drawer are stale. */
  onSaved?: () => void
}

const PREVIEW_DEBOUNCE_MS = 350

const ENDPOINTS = {
  user: {
    effective: getUserPolicyEffective,
    preview: previewUserPolicy,
    put: putUserPolicy,
    remove: deleteUserPolicy,
  },
  api_key: {
    effective: getKeyPolicyEffective,
    preview: previewKeyPolicy,
    put: putKeyPolicy,
    remove: deleteKeyPolicy,
  },
} as const

function errorText(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback
}

/** "1 model" / "3 models" — said the same way everywhere a count of models is
 *  shown, because "1 models" reads as a bug in the policy, not in the copy. */
function modelCount(n: number): string {
  return `${n} ${n === 1 ? 'model' : 'models'}`
}

/**
 * What a key stops being allowed: before minus after. These are allowlists —
 * the preview's own policy merge, not the live reachable set — so a backend
 * that happens to be down never shows up here as a key "losing" a model.
 *
 * A null list is "every model", the widest thing a policy can say. So a null
 * `after` loses nothing whatever `before` was, and a null `before` is measured
 * against every enabled model — the same set an explicit list is seeded from.
 * The preview returns one entry per key the edited user owns, changed or not,
 * so this difference is the only thing that says whether a key actually loses
 * something.
 */
function lostModels(
  before: string[] | null | undefined,
  after: string[] | null | undefined,
  everyModel: string[],
): string[] {
  if (after == null) return []
  const from = before ?? everyModel
  return from.filter((id) => !after.includes(id))
}

/** The other direction: what a key becomes allowed that it was not before. */
function gainedModels(
  before: string[] | null | undefined,
  after: string[] | null | undefined,
  everyModel: string[],
): string[] {
  if (before == null) return []
  const to = after ?? everyModel
  return to.filter((id) => !before.includes(id))
}

// ---------------------------------------------------------------------------
// Small parts
// ---------------------------------------------------------------------------

/** Which level a field's value came from. Sits beside the value, never instead
 *  of it: the source explains the value, it is not the value. A preview that
 *  did not say keeps the chip's place with a dash rather than claiming 'Unset',
 *  which would read as a policy fact rather than a missing one. */
function SourceChip({ source }: { source?: PolicySource }) {
  if (!source) {
    return (
      <span className={styles.source} title="Not reported by this preview">
        —
      </span>
    )
  }
  return <span className={styles.source}>{sourceChip(source)}</span>
}

/**
 * One option of a two-way choice. It holds only text: anything interactive
 * that belongs to the option — the model picker — is rendered as a sibling of
 * the label, never inside it. A control nested in a label is activated twice
 * and, when it is itself a label, is invalid markup besides.
 */
function FieldChoice({
  name,
  checked,
  onSelect,
  label,
  hint,
}: {
  name: string
  checked: boolean
  onSelect: () => void
  label: string
  hint?: string
}) {
  return (
    <label className={`${styles.choice} ${checked ? styles.choiceChecked : ''}`}>
      <span className={styles.choiceTop}>
        <input
          type="radio"
          className={styles.radio}
          name={name}
          checked={checked}
          onChange={onSelect}
        />
        <span className={styles.choiceLabel}>{label}</span>
      </span>
      {hint && <span className={styles.choiceHint}>{hint}</span>}
    </label>
  )
}

// ---------------------------------------------------------------------------
// Drawer
// ---------------------------------------------------------------------------

export function PrincipalPolicyDrawer({ principal, models, aliases, onClose, onSaved }: Props) {
  const { toast } = useToast()
  const open = principal !== null

  const [loading, setLoading] = useState(true)
  /** Which principal the state below belongs to. The load runs in an effect,
   *  which is after paint, so without this the drawer shows the previous row's
   *  answer for a frame when it is reopened on another one. */
  const [loadedFor, setLoadedFor] = useState<string | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [stored, setStored] = useState<PrincipalPolicy | null>(null)
  const [draft, setDraft] = useState<PrincipalPolicy>(INHERIT_ALL)

  const [preview, setPreview] = useState<PolicyPreview | null>(null)
  const [previewing, setPreviewing] = useState(false)
  const [previewError, setPreviewError] = useState<string | null>(null)

  const [saving, setSaving] = useState(false)
  const [resetting, setResetting] = useState(false)

  const enabled = useMemo(
    () => models.filter((m) => m.enabled).sort((a, b) => a.model_id.localeCompare(b.model_id)),
    [models],
  )
  /** Aliases that can contribute concrete ids. A routed alias contributes every
   *  target it can reach — a policy must allow all of them or the route is
   *  refused rather than swapped. */
  const aliasHelpers = useMemo(
    () => aliases.map((a) => ({ alias: a.alias, targets: aliasTargetIds(a) })).filter((a) => a.targets.length > 0),
    [aliases],
  )

  /** Every enabled model, sorted. The fallback for "no restriction is known
   *  yet": what a key's before/after is measured against when a preview
   *  reports null (inherits everything), and what "Choose models" seeds from
   *  until the principal's own effective allowlist has loaded. Once it has,
   *  the actual inherited allowlist is used instead — see the onSelect below. */
  const inheritedAllowed = useMemo(
    () => enabled.map((m) => m.model_id).sort(),
    [enabled],
  )

  const isUser = principal?.type === 'user'
  const endpoints = principal ? ENDPOINTS[principal.type] : null

  /** Who this principal inherits from, named the same way in every sentence on
   *  the drawer — mid-sentence and at the start of one. */
  const inheritFrom = isUser ? 'the organisation' : "this key's owner"
  const inheritFromCapitalised = isUser ? 'The organisation' : "This key's owner"

  // --- load -----------------------------------------------------------------

  const loadSeqRef = useRef(0)

  const load = useCallback(async () => {
    if (!principal) return
    const seq = ++loadSeqRef.current
    setLoading(true)
    setLoadError(null)
    try {
      const result = await ENDPOINTS[principal.type].effective(principal.id)
      if (seq !== loadSeqRef.current) return
      const row = result.stored ?? null
      setStored(row)
      setDraft(row ?? INHERIT_ALL)
      // The merged result for what is stored today, so the panel has something
      // true to show before the first preview comes back.
      setPreview({ effective: result, keys_affected: [], would_lock_out: [] })
      setPreviewError(null)
    } catch (err) {
      if (seq !== loadSeqRef.current) return
      setLoadError(errorText(err, 'This policy could not be read'))
    } finally {
      if (seq === loadSeqRef.current) {
        setLoadedFor(principal.id)
        setLoading(false)
      }
    }
  }, [principal])

  useEffect(() => {
    if (!principal) return
    setPreview(null)
    setPreviewError(null)
    void load()
  }, [principal, load])

  /** The drawer is open on a principal whose policy has not been read yet. It
   *  shows the skeleton until it has one: nothing below it is true before. */
  const busy = loading || (principal !== null && loadedFor !== principal.id)

  // --- live impact ----------------------------------------------------------
  //
  // Every edit asks the API what it would do, without writing it. The panel
  // keeps the last answer while the next one is in flight, so a keystroke never
  // empties the thing the person is reading.

  const draftKey = JSON.stringify(draft)
  const seqRef = useRef(0)

  useEffect(() => {
    if (!principal || busy || loadError) return
    const seq = ++seqRef.current
    const body = JSON.parse(draftKey) as PrincipalPolicy
    const timer = setTimeout(() => {
      setPreviewing(true)
      ENDPOINTS[principal.type]
        .preview(principal.id, body)
        .then((result) => {
          if (seq !== seqRef.current) return
          setPreview(result)
          setPreviewError(null)
        })
        .catch((err: unknown) => {
          if (seq !== seqRef.current) return
          setPreviewError(errorText(err, 'The effect of this change could not be checked'))
        })
        .finally(() => {
          if (seq === seqRef.current) setPreviewing(false)
        })
    }, PREVIEW_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [principal, draftKey, busy, loadError])

  // --- derived --------------------------------------------------------------

  /** Partial by type: a preview may answer without the parts computed from live
   *  backend state, and every read below is written for that. */
  const effective: PreviewEffectivePolicy | null = preview?.effective ?? null
  const reachable = effective?.reachable_models
  const keysAffected = preview?.keys_affected ?? []
  const wouldLockOut = preview?.would_lock_out ?? []
  const lockedOut = wouldLockOut.length > 0

  /** Every key the preview reported, with what it loses and gains. The preview
   *  lists all of the user's keys, so "affected" here is a fact to compute, not
   *  one to assume from the length of the list. */
  const keyImpacts = useMemo(
    () =>
      keysAffected.map((k) => ({
        ...k,
        lost: lostModels(k.before, k.after, inheritedAllowed),
        gained: gainedModels(k.before, k.after, inheritedAllowed),
      })),
    [keysAffected, inheritedAllowed],
  )
  /** The only keys the warning is about: the ones that end up reaching less. */
  const keysLosing = useMemo(() => keyImpacts.filter((k) => k.lost.length > 0), [keyImpacts])
  const keysGaining = useMemo(
    () => keyImpacts.filter((k) => k.lost.length === 0 && k.gained.length > 0),
    [keyImpacts],
  )

  /**
   * The rungs this principal may stand on.
   *
   * A principal only ever narrows, so a rung at or below what it already
   * inherits does nothing — it is offered, disabled, with the reason attached,
   * rather than hidden. Hiding it makes the rule invisible and leaves the
   * administrator wondering whether this screen can do what the other one did;
   * showing it dead states the rule at the moment it applies.
   *
   * The loosest rung is never offered: it is a widening by definition, and no
   * principal-level row can grant it.
   */
  const residencyRungs: LadderRung[] = useMemo(() => {
    const inherited = effective?.residency
    return PRINCIPAL_RESIDENCIES.map((value) => {
      const dead =
        inherited !== undefined && residencyRank(value) <= residencyRank(inherited)
          ? `${inheritFromCapitalised} already limits this to ${residencyLabel(inherited).toLowerCase()}, which is the same or tighter. A policy here can only narrow further.`
          : null
      return { value: value as Residency, deadReason: dead }
    })
  }, [effective?.residency, inheritFromCapitalised])

  /** What the default-model control may offer. A default outside the allowlist
   *  is refused by the API, so it is never on the menu here. */
  const defaultChoices = useMemo(() => {
    const allowed = draft.allowed_models
    const modelOptions = allowed === null ? enabled.map((m) => m.model_id) : allowed
    const aliasOptions = aliasHelpers
      .filter((a) => allowed === null || a.targets.every((t) => allowed.includes(t)))
      .map((a) => a.alias)
    return { modelOptions, aliasOptions }
  }, [draft.allowed_models, enabled, aliasHelpers])

  /** The stored default no longer sits inside the stored allowlist. The API
   *  refuses this, so the drawer says why before the save button is reached. */
  const defaultOutsideAllowlist =
    draft.default_model !== null &&
    draft.allowed_models !== null &&
    !draft.allowed_models.includes(draft.default_model) &&
    !defaultChoices.aliasOptions.includes(draft.default_model)

  const emptyAllowlist = draft.allowed_models !== null && draft.allowed_models.length === 0
  const changed = !samePolicy(draft, stored ?? INHERIT_ALL)
  const canSave =
    !busy && !loadError && changed && !saving && !lockedOut && !defaultOutsideAllowlist && !emptyAllowlist

  // --- editing --------------------------------------------------------------

  const setAllowed = (next: string[] | null) => {
    setDraft((prev) => ({ ...prev, allowed_models: next === null ? null : [...next].sort() }))
  }

  // --- writing --------------------------------------------------------------

  const handleSave = async () => {
    if (!principal || !endpoints) return
    setSaving(true)
    try {
      await endpoints.put(principal.id, draft)
      toast(`Routing policy saved for ${principal.title}`, 'success')
      onSaved?.()
      onClose()
    } catch (err) {
      toast(errorText(err, 'The policy could not be saved'), 'error')
    } finally {
      setSaving(false)
    }
  }

  const handleReset = async () => {
    if (!principal || !endpoints) return
    setResetting(true)
    try {
      await endpoints.remove(principal.id)
      toast(
        `${principal.title} inherits ${isUser ? "the organisation's policy" : "its owner's policy"} again`,
        'success',
      )
      onSaved?.()
      onClose()
    } catch (err) {
      toast(errorText(err, 'The policy could not be reset'), 'error')
    } finally {
      setResetting(false)
    }
  }

  const dialogTitle = principal ? `Routing policy for ${principal.title}` : 'Routing policy'

  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.overlay} />
        <Dialog.Content className={styles.drawer} aria-describedby={undefined}>
          <VisuallyHidden.Root asChild>
            <Dialog.Title>{dialogTitle}</Dialog.Title>
          </VisuallyHidden.Root>

          <Dialog.Close asChild>
            <button type="button" className={styles.closeBtn} aria-label="Close">
              <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                <line x1="3" y1="3" x2="11" y2="11" />
                <line x1="11" y1="3" x2="3" y2="11" />
              </svg>
            </button>
          </Dialog.Close>

          <header className={styles.header}>
            <p className={styles.eyebrow}>Routing policy</p>
            <h2 className={styles.name}>{principal?.title}</h2>
            <p className={styles.subtitle}>{principal?.subtitle}</p>
            <p className={styles.rule}>{POLICY_RULE}</p>
          </header>

          {busy ? (
            <div className={styles.body}>
              <Skeleton width="100%" height="92px" borderRadius="var(--radius-md)" />
              <Skeleton width="100%" height="180px" borderRadius="var(--radius-md)" />
              <Skeleton width="100%" height="92px" borderRadius="var(--radius-md)" />
            </div>
          ) : loadError ? (
            <div className={styles.body}>
              <div className={styles.loadError} role="alert">
                <p className={styles.loadErrorTitle}>This policy could not be read</p>
                <p className={styles.loadErrorDetail}>{loadError}</p>
                <Button variant="secondary" size="sm" onClick={() => void load()}>Try again</Button>
              </div>
            </div>
          ) : (
            <>
              <div className={styles.body}>
                {/* --- Residency --- */}
                <ResidencyLadder
                  legend="Where requests may run"
                  name="policy-residency"
                  value={draft.residency}
                  rungs={residencyRungs}
                  disabled={saving || resetting}
                  inherit={{
                    label: `Inherit from ${inheritFrom}`,
                    hint: effective
                      ? `Currently ${residencyLabel(effective.residency).toLowerCase()}.`
                      : undefined,
                  }}
                  onChange={(next) =>
                    setDraft((p) => ({ ...p, residency: next as PrincipalResidency | null }))
                  }
                />

                {/* --- Allowed models --- */}
                <fieldset className={styles.field}>
                  <legend className={styles.legend}>Which models may answer</legend>
                  <div className={styles.choices}>
                    <FieldChoice
                      name="policy-allowed"
                      checked={draft.allowed_models === null}
                      onSelect={() => setAllowed(null)}
                      label={`Inherit from ${inheritFrom}`}
                      hint={
                        effective?.allowed_models === null
                          ? 'Every enabled model.'
                          : effective
                            ? `${effective.allowed_models?.length ?? 0} models.`
                            : undefined
                      }
                    />
                    <FieldChoice
                      name="policy-allowed"
                      checked={draft.allowed_models !== null}
                      /* Seeded with what is inherited, so switching to an
                         explicit list changes nothing until a box is unticked:
                         this principal's own list if it already has one,
                         otherwise the inherited allowlist as the last preview
                         computed it (a policy set, not the reachable set — a
                         down backend must not quietly drop a model from what
                         gets ticked), and only every enabled model as a last
                         resort before that preview has ever come back. */
                      onSelect={() => setAllowed(stored?.allowed_models ?? effective?.allowed_models ?? inheritedAllowed)}
                      label="Choose models"
                      hint="Only the models ticked here, and never more than what is inherited."
                    />
                  </div>

                  {draft.allowed_models !== null && (
                    <div className={styles.picker}>
                      <ModelAllowlistPicker
                        models={models}
                        aliases={aliases}
                        value={draft.allowed_models}
                        onChange={setAllowed}
                        label={principal?.title ?? 'this principal'}
                      />
                      {emptyAllowlist && (
                        <p className={styles.fieldError} role="alert">
                          Tick at least one model. An empty list is refused — use Reset to inherit
                          instead.
                        </p>
                      )}
                    </div>
                  )}
                </fieldset>

                {/* --- Default model --- */}
                <fieldset className={styles.field}>
                  <legend className={styles.legend}>Default model</legend>
                  <p className={styles.fieldNote}>
                    Used when an API request names no model, and preselected in the chat picker.
                  </p>
                  <select
                    className={styles.select}
                    aria-label="Default model"
                    value={draft.default_model ?? ''}
                    onChange={(e) =>
                      setDraft((p) => ({ ...p, default_model: e.target.value || null }))
                    }
                  >
                    <option value="">
                      Inherit from {inheritFrom}
                      {effective?.default_model ? ` — ${modelLabel(effective.default_model, models)}` : ' — none set'}
                    </option>
                    {defaultChoices.aliasOptions.length > 0 && (
                      <optgroup label="Shortcuts">
                        {defaultChoices.aliasOptions.map((a) => (
                          <option key={a} value={a}>{a}</option>
                        ))}
                      </optgroup>
                    )}
                    <optgroup label="Models">
                      {defaultChoices.modelOptions.map((id) => (
                        <option key={id} value={id}>{modelLabel(id, models)}</option>
                      ))}
                    </optgroup>
                  </select>
                  {defaultOutsideAllowlist && (
                    <p className={styles.fieldError} role="alert">
                      <code>{draft.default_model}</code> is not in the allowed list. Tick it above,
                      or choose a different default.
                    </p>
                  )}
                </fieldset>

                {/* --- What this comes to --- */}
                <section className={styles.effect} aria-label="Effective policy">
                  <div className={styles.effectHead}>
                    <h3 className={styles.effectTitle}>What this comes to</h3>
                    <span className={styles.effectState} role="status" aria-live="polite">
                      {previewing ? 'Checking…' : stored ? 'Stored policy applied' : 'Inheriting'}
                    </span>
                  </div>

                  {previewError && (
                    <p className={styles.previewError} role="alert">{previewError}</p>
                  )}

                  {effective && (
                    <dl className={styles.effectGrid}>
                      <dt>Where it runs</dt>
                      <dd>
                        {residencyLabel(effective.residency)}
                        <SourceChip source={effective.sources?.residency} />
                      </dd>

                      <dt>Models allowed</dt>
                      <dd>
                        {effective.allowed_models === null
                          ? 'Every enabled model'
                          : `${effective.allowed_models.length} ${effective.allowed_models.length === 1 ? 'model' : 'models'}`}
                        <SourceChip source={effective.sources?.allowed_models} />
                      </dd>

                      <dt>Default model</dt>
                      <dd>
                        {effective.default_model
                          ? modelLabel(effective.default_model, models)
                          : 'None — a request must name a model'}
                        <SourceChip source={effective.sources?.default_model} />
                      </dd>

                      {/* Reachability is computed from live backend state. A
                          preview that did not report it says nothing here,
                          rather than claiming nothing is reachable. */}
                      {reachable !== undefined && (
                        <>
                          <dt>Reachable now</dt>
                          <dd>
                            {reachable.length === 0 ? (
                              <span className={styles.none}>Nothing</span>
                            ) : (
                              <span className={styles.chips}>
                                {reachable.slice(0, 8).map((id) => (
                                  <code key={id} className={styles.chip}>{modelLabel(id, models)}</code>
                                ))}
                                {reachable.length > 8 && (
                                  <span className={styles.chipMore}>
                                    and {reachable.length - 8} more
                                  </span>
                                )}
                              </span>
                            )}
                          </dd>
                        </>
                      )}
                    </dl>
                  )}

                  <p className={styles.effectNote}>
                    Allowed is what the policy permits. Reachable is what is allowed, enabled and
                    answering right now.
                  </p>
                </section>
              </div>

              <footer className={styles.footer}>
                <div className={styles.impact} role="status" aria-live="polite">
                  {lockedOut ? (
                    <p className={styles.impactBlock}>
                      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                        <circle cx="12" cy="12" r="10" />
                        <line x1="12" y1="8" x2="12" y2="13" />
                        <line x1="12" y1="16" x2="12.01" y2="16" />
                      </svg>
                      <span>
                        {isUser
                          ? `This would leave ${wouldLockOut.length === 1 ? wouldLockOut[0] : `${wouldLockOut.length} of this user's keys`} with no model at all — save disabled.`
                          : 'This would lock this key out of every model — save disabled.'}
                      </span>
                    </p>
                  ) : keysLosing.length > 0 ? (
                    <div className={styles.impactWarn}>
                      <p>
                        {!isUser
                          ? `This will drop ${modelCount(keysLosing[0].lost.length)} from what this key is allowed.`
                          : keysLosing.length === 1
                            ? "This will narrow what 1 of this user's keys is allowed."
                            : `This will narrow what ${keysLosing.length} of this user's keys are allowed.`}
                      </p>
                      <ul className={styles.impactList}>
                        {keysLosing.slice(0, 4).map((k) => (
                          <li key={k.id}>
                            <span className={styles.impactKeyRow}>
                              <span className={styles.impactKey}>{k.name}</span>
                              <span className={styles.impactCounts}>
                                loses {modelCount(k.lost.length)}
                              </span>
                            </span>
                            <span className={styles.impactLost}>
                              {k.lost.slice(0, 4).map((id) => modelLabel(id, models)).join(', ')}
                              {k.lost.length > 4 && ` and ${k.lost.length - 4} more`}
                            </span>
                          </li>
                        ))}
                      </ul>
                      {keysLosing.length > 4 && (
                        <p className={styles.impactMore}>and {keysLosing.length - 4} more</p>
                      )}
                    </div>
                  ) : changed ? (
                    <p className={styles.impactQuiet}>
                      {isUser ? 'No key loses access.' : 'This key is allowed no less than it was.'}
                      {keysGaining.length > 0 &&
                        ` ${keysGaining.length === 1 ? '1 key is allowed' : `${keysGaining.length} keys are allowed`} more than before.`}
                    </p>
                  ) : (
                    <p className={styles.impactQuiet}>
                      {stored ? 'This principal has a policy of its own.' : `Inherits everything from ${inheritFrom}.`}
                    </p>
                  )}
                </div>

                <p className={styles.timing}>{POLICY_TIMING}</p>

                <div className={styles.actions}>
                  {stored && !isInheritAll(stored) && (
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => void handleReset()}
                      disabled={resetting || saving}
                      className={styles.resetBtn}
                    >
                      {resetting ? 'Resetting…' : 'Reset to inherit'}
                    </Button>
                  )}
                  <Button variant="ghost" size="sm" onClick={onClose} disabled={saving || resetting}>
                    Cancel
                  </Button>
                  <Button size="sm" onClick={() => void handleSave()} disabled={!canSave}>
                    {saving ? 'Saving…' : 'Save policy'}
                  </Button>
                </div>
              </footer>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
