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
  aliasTargetIds,
  isInheritAll,
  modelLabel,
  residencyLabel,
  samePolicy,
  sourceChip,
} from '../lib/policy'
import type {
  AdminModel,
  ModelAlias,
  PolicyPreview,
  PolicySource,
  PreviewEffectivePolicy,
  PrincipalPolicy,
} from '../types'
import { useToast } from '../components/Toast'
import { Button } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ModelAllowlistPicker } from './ModelAllowlistPicker'
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

  /** What an explicit list starts from: the inherited allowlist if there is
   *  one, otherwise every enabled model. */
  const inheritedAllowed = useMemo(
    () => enabled.map((m) => m.model_id).sort(),
    [enabled],
  )

  const isUser = principal?.type === 'user'
  const endpoints = principal ? ENDPOINTS[principal.type] : null

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

  const inheritFrom = isUser ? 'the organisation' : "this key's owner"
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
                <fieldset className={styles.field}>
                  <legend className={styles.legend}>Where requests may run</legend>
                  <div className={styles.choices}>
                    <FieldChoice
                      name="policy-residency"
                      checked={draft.residency === null}
                      onSelect={() => setDraft((p) => ({ ...p, residency: null }))}
                      label={`Inherit from ${inheritFrom}`}
                      hint={
                        effective
                          ? `Currently ${residencyLabel(effective.residency).toLowerCase()}.`
                          : undefined
                      }
                    />
                    <FieldChoice
                      name="policy-residency"
                      checked={draft.residency === 'self_hosted_only'}
                      onSelect={() => setDraft((p) => ({ ...p, residency: 'self_hosted_only' }))}
                      label="Self-hosted only"
                      hint="Cloud-managed models such as AWS Bedrock are refused, even when they are enabled and allowed."
                    />
                  </div>
                </fieldset>

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
                         explicit list changes nothing until a box is unticked.
                         Seeding from the reachable set instead would quietly
                         drop any model whose backend happens to be down. */
                      onSelect={() => setAllowed(stored?.allowed_models ?? inheritedAllowed)}
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
                  ) : keysAffected.length > 0 ? (
                    <div className={styles.impactWarn}>
                      <p>
                        {keysAffected.length === 1
                          ? "This will block 1 of this user's keys from models it can reach today."
                          : `This will block ${keysAffected.length} of this user's keys from models they can reach today.`}
                      </p>
                      <ul className={styles.impactList}>
                        {keysAffected.slice(0, 4).map((k) => (
                          <li key={k.id}>
                            <span className={styles.impactKey}>{k.name}</span>
                            <span className={styles.impactCounts}>
                              {(k.before ?? []).length} → {(k.after ?? []).length} models
                            </span>
                          </li>
                        ))}
                      </ul>
                      {keysAffected.length > 4 && (
                        <p className={styles.impactMore}>and {keysAffected.length - 4} more</p>
                      )}
                    </div>
                  ) : changed ? (
                    <p className={styles.impactQuiet}>No key loses access because of this change.</p>
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
