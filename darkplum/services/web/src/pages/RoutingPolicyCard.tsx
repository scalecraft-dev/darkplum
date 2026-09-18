import { useEffect, useMemo, useState } from 'react'
import type { AdminModel, ModelAlias, Residency, RoutingPolicy } from '../types'
import { backendClassOf } from '../lib/modelRouting'
import { POLICY_RULE, POLICY_TIMING, aliasTargetIds, modelLabel } from '../lib/policy'
import { Button, Modal } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ModelAllowlistPicker } from './ModelAllowlistPicker'
import styles from './RoutingPolicyCard.module.css'

/** The three fields of the organisation's policy, as the card edits them. */
export interface OrgPolicyDraft {
  residency: Residency
  allowed_models: string[] | null
  default_model: string | null
}

interface Props {
  policy: RoutingPolicy | null
  loading: boolean
  error: string | null
  saving: boolean
  /** Every discovered model — used to say what a tighter policy would block. */
  models: AdminModel[]
  /** Aliases, for the allowlist helper and the default-model menu. */
  aliases: ModelAlias[]
  onRetry: () => void
  /** Resolves with what the API said the new policy does. */
  onChange: (draft: OrgPolicyDraft) => Promise<{ warnings: string[]; affectedPrincipals: number }>
}

const RESIDENCY_OPTIONS: { value: Residency; label: string; consequence: string }[] = [
  {
    value: 'any',
    label: 'Any model',
    consequence:
      'Requests can run on self-hosted models and on cloud-managed models such as AWS Bedrock.',
  },
  {
    value: 'self_hosted_only',
    label: 'Self-hosted only',
    consequence:
      'Requests to cloud-managed models such as Bedrock will be refused, even when the model is enabled.',
  },
]

function toDraft(policy: RoutingPolicy | null): OrgPolicyDraft {
  return {
    residency: policy?.residency ?? 'any',
    allowed_models: policy?.allowed_models ?? null,
    default_model: policy?.default_model ?? null,
  }
}

function sameDraft(a: OrgPolicyDraft, b: OrgPolicyDraft): boolean {
  if (a.residency !== b.residency || a.default_model !== b.default_model) return false
  if (a.allowed_models === null || b.allowed_models === null) {
    return a.allowed_models === b.allowed_models
  }
  return (
    a.allowed_models.length === b.allowed_models.length &&
    a.allowed_models.every((id, i) => id === b.allowed_models![i])
  )
}

export function RoutingPolicyCard({
  policy,
  loading,
  error,
  saving,
  models,
  aliases,
  onRetry,
  onChange,
}: Props) {
  const saved = useMemo(() => toDraft(policy), [policy])
  const [draft, setDraft] = useState<OrgPolicyDraft>(saved)
  const [confirmOpen, setConfirmOpen] = useState(false)
  /** What the API said the new policy breaks. Written by the API, shown as it
   *  came: these name routed aliases that will start refusing. */
  const [warnings, setWarnings] = useState<string[]>([])
  /** How many users and keys reach less than they did. Written by the API. */
  const [affected, setAffected] = useState<number | null>(null)

  // A reload or a save replaces what is stored; the form follows it rather than
  // keeping an edit the server has never heard of.
  useEffect(() => { setDraft(saved) }, [saved])

  const enabled = useMemo(() => models.filter((m) => m.enabled), [models])
  const cloudEnabled = useMemo(
    () => enabled.filter((m) => backendClassOf(m) === 'cloud_managed'),
    [enabled],
  )

  const aliasOptions = useMemo(
    () =>
      aliases
        .map((a) => ({ alias: a.alias, targets: aliasTargetIds(a) }))
        .filter(
          (a) =>
            a.targets.length > 0 &&
            (draft.allowed_models === null ||
              a.targets.every((t) => draft.allowed_models!.includes(t))),
        )
        .map((a) => a.alias),
    [aliases, draft.allowed_models],
  )

  const modelOptions = draft.allowed_models ?? enabled.map((m) => m.model_id)

  const defaultOutsideAllowlist =
    draft.default_model !== null &&
    draft.allowed_models !== null &&
    !draft.allowed_models.includes(draft.default_model) &&
    !aliasOptions.includes(draft.default_model)

  const emptyAllowlist = draft.allowed_models !== null && draft.allowed_models.length === 0
  const changed = !sameDraft(draft, saved)

  /** Tightening is anything that can take access away from someone who has it
   *  now: a stricter residency, or a list where there was none, or a shorter
   *  one. Those get a confirmation; widening does not. */
  const tightening =
    (draft.residency === 'self_hosted_only' && saved.residency !== 'self_hosted_only') ||
    (draft.allowed_models !== null && saved.allowed_models === null) ||
    (draft.allowed_models !== null &&
      saved.allowed_models !== null &&
      draft.allowed_models.length < saved.allowed_models.length)

  const canSave = changed && !saving && !emptyAllowlist && !defaultOutsideAllowlist

  const commit = async () => {
    const result = await onChange(draft)
    setWarnings(result.warnings)
    setAffected(result.affectedPrincipals)
    if (result.warnings.length === 0) setConfirmOpen(false)
  }

  const handleSave = () => {
    if (!canSave) return
    setWarnings([])
    setAffected(null)
    if (tightening) {
      setConfirmOpen(true)
      return
    }
    void commit()
  }

  const restricted = saved.residency === 'self_hosted_only' || saved.allowed_models !== null

  return (
    <section
      className={`${styles.card} ${restricted ? styles.cardRestricted : ''}`}
      aria-labelledby="routing-policy-heading"
    >
      <div className={styles.head}>
        <div>
          <h2 id="routing-policy-heading" className={styles.title}>Routing policy</h2>
          <p className={styles.subtitle}>
            What the whole organisation may reach. Users and keys can be narrowed further, never
            widened.
          </p>
        </div>
        {policy && (
          <span className={styles.updated}>
            Changed {new Date(policy.updated_at).toLocaleDateString()}
          </span>
        )}
      </div>

      {loading ? (
        <div className={styles.options}>
          <Skeleton width="100%" height="88px" borderRadius="var(--radius-md)" />
          <Skeleton width="100%" height="88px" borderRadius="var(--radius-md)" />
        </div>
      ) : error ? (
        <div className={styles.error} role="alert">
          <p className={styles.errorTitle}>Routing policy could not be loaded</p>
          <p className={styles.errorDetail}>{error}</p>
          <Button variant="secondary" size="sm" onClick={onRetry}>Try again</Button>
        </div>
      ) : (
        <>
          <fieldset className={styles.field}>
            <legend className={styles.legend}>Where requests may run</legend>
            <div className={styles.options} role="radiogroup" aria-label="Where requests may run">
              {RESIDENCY_OPTIONS.map((opt) => {
                const checked = draft.residency === opt.value
                return (
                  <label
                    key={opt.value}
                    className={`${styles.option} ${checked ? styles.optionChecked : ''}`}
                  >
                    <span className={styles.optionTop}>
                      <input
                        className={styles.radio}
                        type="radio"
                        name="residency"
                        value={opt.value}
                        checked={checked}
                        disabled={saving}
                        onChange={() => setDraft((p) => ({ ...p, residency: opt.value }))}
                      />
                      <span className={styles.optionLabel}>{opt.label}</span>
                    </span>
                    <span className={styles.optionConsequence}>{opt.consequence}</span>
                  </label>
                )
              })}
            </div>
          </fieldset>

          {draft.residency === 'self_hosted_only' && (
            <p className={styles.effect}>
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
                <path d="M7 11V7a5 5 0 0 1 10 0v4" />
              </svg>
              {cloudEnabled.length === 0
                ? 'No cloud-managed models are enabled, so nothing is being refused right now.'
                : `${cloudEnabled.length} enabled cloud-managed ${cloudEnabled.length === 1 ? 'model is' : 'models are'} refused by this policy.`}
            </p>
          )}

          <fieldset className={styles.field}>
            <legend className={styles.legend}>Which models may answer</legend>
            <div className={styles.options} role="radiogroup" aria-label="Which models may answer">
              <label
                className={`${styles.option} ${draft.allowed_models === null ? styles.optionChecked : ''}`}
              >
                <span className={styles.optionTop}>
                  <input
                    className={styles.radio}
                    type="radio"
                    name="org-allowed"
                    checked={draft.allowed_models === null}
                    disabled={saving}
                    onChange={() => setDraft((p) => ({ ...p, allowed_models: null }))}
                  />
                  <span className={styles.optionLabel}>Every enabled model</span>
                </span>
                <span className={styles.optionConsequence}>
                  Turning a model on in the list below is all it takes to make it reachable.
                </span>
              </label>
              <label
                className={`${styles.option} ${draft.allowed_models !== null ? styles.optionChecked : ''}`}
              >
                <span className={styles.optionTop}>
                  <input
                    className={styles.radio}
                    type="radio"
                    name="org-allowed"
                    checked={draft.allowed_models !== null}
                    disabled={saving}
                    onChange={() =>
                      setDraft((p) => ({
                        ...p,
                        allowed_models: enabled.map((m) => m.model_id).sort(),
                      }))
                    }
                  />
                  <span className={styles.optionLabel}>Choose models</span>
                </span>
                <span className={styles.optionConsequence}>
                  Only the models ticked here. A model that is enabled but not ticked is refused.
                </span>
              </label>
            </div>

            {draft.allowed_models !== null && (
              <div className={styles.pickerWrap}>
                <ModelAllowlistPicker
                  models={models}
                  aliases={aliases}
                  value={draft.allowed_models}
                  onChange={(next) => setDraft((p) => ({ ...p, allowed_models: next }))}
                  label="the organisation"
                />
                {emptyAllowlist && (
                  <p className={styles.fieldError} role="alert">
                    Tick at least one model. An organisation that allows nothing cannot chat — use
                    "Every enabled model" and disable models instead.
                  </p>
                )}
              </div>
            )}
          </fieldset>

          <fieldset className={styles.field}>
            <legend className={styles.legend}>Default model</legend>
            <p className={styles.fieldNote}>
              Preselected in the chat picker, and used when an API request names no model. Users and
              keys can set their own.
            </p>
            <select
              className={styles.select}
              aria-label="Default model for the organisation"
              value={draft.default_model ?? ''}
              disabled={saving}
              onChange={(e) => setDraft((p) => ({ ...p, default_model: e.target.value || null }))}
            >
              <option value="">No default — a request must name a model</option>
              {aliasOptions.length > 0 && (
                <optgroup label="Shortcuts">
                  {aliasOptions.map((a) => (
                    <option key={a} value={a}>{a}</option>
                  ))}
                </optgroup>
              )}
              <optgroup label="Models">
                {modelOptions.map((id) => (
                  <option key={id} value={id}>{modelLabel(id, models)}</option>
                ))}
              </optgroup>
            </select>
            {defaultOutsideAllowlist && (
              <p className={styles.fieldError} role="alert">
                <code>{draft.default_model}</code> is not in the allowed list. Tick it above, or
                choose a different default.
              </p>
            )}
          </fieldset>

          {affected !== null && affected > 0 && (
            <p className={styles.affected} role="status">
              {affected === 1
                ? '1 user or key now reaches fewer models than before.'
                : `${affected} users and keys now reach fewer models than before.`}{' '}
              Their own policies are unchanged — the organisation moved under them.
            </p>
          )}

          {warnings.length > 0 && (
            <div className={styles.warnings} role="status">
              <p className={styles.warningsTitle}>
                {warnings.length === 1
                  ? 'One route will be refused under this policy:'
                  : `${warnings.length} routes will be refused under this policy:`}
              </p>
              <ul className={styles.warningsList}>
                {warnings.map((w) => (
                  <li key={w}>{w}</li>
                ))}
              </ul>
              <p className={styles.warningsHint}>
                A message that takes one of these routes is refused with the route named. It is not
                sent to a different model instead. Repoint the route, or change the policy.
              </p>
            </div>
          )}

          <footer className={styles.footer}>
            <p className={styles.rule}>{POLICY_RULE}</p>
            <div className={styles.footerActions}>
              {changed && (
                <Button variant="ghost" size="sm" onClick={() => setDraft(saved)} disabled={saving}>
                  Discard changes
                </Button>
              )}
              <Button size="sm" onClick={handleSave} disabled={!canSave}>
                {saving ? 'Saving…' : 'Save policy'}
              </Button>
            </div>
          </footer>
          <p className={styles.timing}>{POLICY_TIMING}</p>
        </>
      )}

      <Modal
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        title={warnings.length > 0 ? 'Policy saved' : 'Narrow what the organisation can reach?'}
        description={
          warnings.length > 0
            ? 'The policy has changed. These routes need your attention.'
            : 'Everyone inherits this. A user or key that already has its own policy keeps it, but can never reach past the organisation.'
        }
      >
        <div className={styles.confirmBody}>
          {warnings.length > 0 ? (
            <>
              <p className={styles.confirmText}>
                {warnings.length === 1
                  ? 'One routed alias has a route that will be refused:'
                  : `${warnings.length} routed-alias routes will be refused:`}
              </p>
              <ul className={styles.confirmList}>
                {warnings.map((w) => (
                  <li key={w}>{w}</li>
                ))}
              </ul>
              <p className={styles.confirmText}>
                A message that takes one of these routes is refused with the route named — it is
                never sent to another model instead.
              </p>
              <div className={styles.confirmFooter}>
                <Button type="button" onClick={() => setConfirmOpen(false)}>Done</Button>
              </div>
            </>
          ) : (
            <>
              {draft.residency === 'self_hosted_only' && cloudEnabled.length > 0 && (
                <>
                  <p className={styles.confirmText}>
                    {cloudEnabled.length} enabled cloud-managed{' '}
                    {cloudEnabled.length === 1 ? 'model stops' : 'models stop'} answering, including
                    anyone using an alias that points at{' '}
                    {cloudEnabled.length === 1 ? 'it' : 'them'}:
                  </p>
                  <ul className={styles.confirmList}>
                    {cloudEnabled.slice(0, 5).map((m) => (
                      <li key={m.id}><code className={styles.confirmModel}>{m.model_id}</code></li>
                    ))}
                  </ul>
                  {cloudEnabled.length > 5 && (
                    <p className={styles.confirmMore}>and {cloudEnabled.length - 5} more</p>
                  )}
                </>
              )}
              {draft.allowed_models !== null && (
                <p className={styles.confirmText}>
                  {draft.allowed_models.length} of {enabled.length} enabled models stay reachable.
                  Everything else is refused, whoever asks for it.
                </p>
              )}
              {draft.residency === 'self_hosted_only' && cloudEnabled.length === 0 && draft.allowed_models === null && (
                <p className={styles.confirmText}>
                  No cloud-managed models are enabled today, so no one loses access right now. The
                  policy still applies to anything you enable later.
                </p>
              )}
              <p className={styles.confirmText}>{POLICY_TIMING}</p>
              <div className={styles.confirmFooter}>
                <Button type="button" variant="ghost" onClick={() => setConfirmOpen(false)}>
                  Cancel
                </Button>
                <Button type="button" onClick={() => void commit()} disabled={saving}>
                  {saving ? 'Saving…' : 'Save policy'}
                </Button>
              </div>
            </>
          )}
        </div>
      </Modal>
    </section>
  )
}
