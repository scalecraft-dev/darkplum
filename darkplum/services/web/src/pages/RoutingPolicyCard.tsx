import { useEffect, useMemo, useState } from 'react'
import type { AdminModel, DisplayClass, ModelAlias, Residency, RoutingPolicy } from '../types'
import {
  backendClassLabel,
  displayClassOf,
  vendorLabel,
} from '../lib/modelRouting'
import {
  POLICY_RULE,
  POLICY_TIMING,
  RESIDENCY_LADDER,
  aliasTargetIds,
  modelLabel,
  orgPolicyRestricted,
  residencyLabel,
  residencyPermits,
  residencyRank,
} from '../lib/policy'
import { Button, EgressGlyph, Modal } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ModelAllowlistPicker } from './ModelAllowlistPicker'
import { ResidencyLadder } from './ResidencyLadder'
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
  /** Resolves with what the API said the new policy does — or, when the API
   *  refused it, who it would have left with no model (`lockedOut`). */
  onChange: (draft: OrgPolicyDraft) => Promise<{
    warnings: string[]
    /** Principals with a policy of their own whose reachable set actually
     *  shrank — not merely how many have a row. */
    affectedPrincipals: number
    /** Whether everyone who simply inherits the organisation's policy now
     *  reaches fewer models too. Independent of affectedPrincipals. */
    orgNarrowed: boolean
    /** Enabled, allowed models no backend has reported yet (class unknown)
     *  under a self-hosted-only policy. Neither counted lost nor kept. */
    unresolvedModels: string[]
    lockedOut: string[]
    /** The API refused the save for a reason that is not a lockout — a routed
     *  alias with a route the new rung would refuse, say. Its own sentence,
     *  shown as it came: the UI does not read it, it relays it. */
    refusal: string | null
  }>
}

/**
 * What a confirmation is for. Narrowing and widening are not the same question
 * asked in two directions: one takes access away from people who have it, the
 * other sends prompts somewhere they have never been. They get different
 * titles, different bodies and differently weighted buttons, and `warnings` is
 * the third case — the save already happened and the API had something to say
 * about it.
 */
type ConfirmKind = 'none' | 'tighten' | 'loosen' | 'warnings'

/** The organisation may stand on any rung. Nothing above it can forbid one. */
const ORG_RUNGS = RESIDENCY_LADDER.map((r) => ({ value: r.value, deadReason: null }))

/** Which classes a refusal covers, named. A count on its own leaves an
 *  administrator opening the catalogue to find out what it was counting. */
function refusedClassSummary(refused: AdminModel[]): string {
  const classes = [...new Set(refused.map((m) => displayClassOf(m)))] as DisplayClass[]
  const named = classes
    .filter((c) => c !== 'unknown')
    .map((c) => backendClassLabel(c).toLowerCase())
  if (named.length === 0) return ''
  return named.length === 1
    ? `They are ${named[0]} models.`
    : `They are ${named.slice(0, -1).join(', ')} and ${named[named.length - 1]} models.`
}

function toDraft(policy: RoutingPolicy | null): OrgPolicyDraft {
  return {
    // No policy read yet means no opinion, not "allow everything": the widest
    // rung is never the one a blank form defaults to.
    residency: policy?.residency ?? 'cloud_account_only',
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
  const [confirmKind, setConfirmKind] = useState<ConfirmKind>('none')
  /** What the API said the new policy breaks. Written by the API, shown as it
   *  came: these name routed aliases that will start refusing. */
  const [warnings, setWarnings] = useState<string[]>([])
  /** How many users and keys with a policy of their own reach less than they
   *  did. Written by the API. */
  const [affected, setAffected] = useState<number | null>(null)
  /** Whether everyone who inherits the organisation's policy reaches less too.
   *  Independent of `affected` — a principal with its own row is never
   *  counted here even if the org narrowed under it as well. */
  const [orgNarrowed, setOrgNarrowed] = useState(false)
  /** Models the count could not judge (see onChange). Said separately, so the
   *  sentences above state only what is certain. */
  const [unresolved, setUnresolved] = useState<string[]>([])
  /** Who the API said the last attempt would have locked out of every model.
   *  Non-empty means the save was refused and nothing changed. */
  const [lockedOut, setLockedOut] = useState<string[]>([])
  /** A refusal the card has no structure for — the API's sentence, relayed. */
  const [refusal, setRefusal] = useState<string | null>(null)

  // A reload or a save replaces what is stored; the form follows it rather than
  // keeping an edit the server has never heard of.
  useEffect(() => { setDraft(saved) }, [saved])

  const enabled = useMemo(() => models.filter((m) => m.enabled), [models])

  /** Enabled models the draft rung refuses: what choosing it costs, counted
   *  from the catalogue rather than described in the abstract. */
  const refusedByDraft = useMemo(
    () => enabled.filter((m) => !residencyPermits(draft.residency, displayClassOf(m))),
    [enabled, draft.residency],
  )

  /** Enabled models the stored rung refuses and the draft one would not: what
   *  loosening actually opens up. Empty is the ordinary case when a vendor key
   *  has not been installed yet, and the confirmation says so rather than
   *  claiming an effect it cannot point at. */
  const openedByDraft = useMemo(
    () =>
      enabled.filter(
        (m) =>
          !residencyPermits(saved.residency, displayClassOf(m)) &&
          residencyPermits(draft.residency, displayClassOf(m)),
      ),
    [enabled, saved.residency, draft.residency],
  )

  /** The vendors that would start receiving prompts, named. "Outside your
   *  account" is not something an administrator can take to their security
   *  team; "Anthropic" is. */
  const openedVendors = useMemo(
    () => [...new Set(openedByDraft.map((m) => m.provider))].map(vendorLabel).sort(),
    [openedByDraft],
  )

  /** Enabled models no backend has reported a class for. They are refused
   *  under every rung, including the loosest, so moving the ladder does not
   *  change them and the card says that on its own line. */
  const unknownEnabled = useMemo(
    () => enabled.filter((m) => displayClassOf(m) === 'unknown'),
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
   *  now: a higher rung, or a list where there was none, or a shorter one. */
  const tightening =
    residencyRank(draft.residency) > residencyRank(saved.residency) ||
    (draft.allowed_models !== null && saved.allowed_models === null) ||
    (draft.allowed_models !== null &&
      saved.allowed_models !== null &&
      draft.allowed_models.length < saved.allowed_models.length)

  /**
   * Loosening the rung is its own kind of change, and the only one in the
   * product where saying yes sends prompts out of the organisation's cloud
   * account. It is confirmed even though nobody loses anything — a
   * confirmation is not a punishment for narrowing, it is for a decision that
   * is hard to see and hard to undo. Widening an allowlist is neither, and is
   * not confirmed.
   */
  const loosening = residencyRank(draft.residency) < residencyRank(saved.residency)

  /** Only the step onto the loosest rung crosses the account boundary. Dropping
   *  from self-hosted-only to cloud-account-only is a widening, but everything
   *  it opens is still inside the organisation's own account, so it does not
   *  get the danger treatment. */
  const leavingAccount = loosening && residencyRank(draft.residency) === 0

  const canSave = changed && !saving && !emptyAllowlist && !defaultOutsideAllowlist

  const commit = async () => {
    const result = await onChange(draft)
    setLockedOut(result.lockedOut)
    setRefusal(result.refusal)
    if (result.lockedOut.length > 0 || result.refusal !== null) {
      // Refused: the form stays ahead of what is stored, and the box below
      // says either who would have lost everything, or — when the API refused
      // for some other reason — what it said.
      setConfirmKind('none')
      return
    }
    setWarnings(result.warnings)
    setAffected(result.affectedPrincipals)
    setOrgNarrowed(result.orgNarrowed)
    setUnresolved(result.unresolvedModels)
    // The save landed. Anything the API had to say about it keeps the modal
    // open and turns it into the report; nothing to say closes it.
    setConfirmKind(result.warnings.length > 0 ? 'warnings' : 'none')
  }

  const handleSave = () => {
    if (!canSave) return
    setWarnings([])
    setAffected(null)
    setOrgNarrowed(false)
    setUnresolved([])
    setLockedOut([])
    setRefusal(null)
    if (loosening) {
      setConfirmKind('loosen')
      return
    }
    if (tightening) {
      setConfirmKind('tighten')
      return
    }
    void commit()
  }

  const restricted = orgPolicyRestricted(saved)

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
          <div className={styles.headMeta}>
            {/* What is stored right now, not what the form is showing. An
                administrator part-way through an edit still needs to be able
                to read what is in force. */}
            <span
              className={`${styles.currentRung} ${
                residencyRank(saved.residency) === 0 ? styles.currentRungEgress : ''
              }`}
            >
              {residencyRank(saved.residency) === 0 && <EgressGlyph />}
              {residencyLabel(saved.residency)}
            </span>
            <span className={styles.updated}>
              Changed {new Date(policy.updated_at).toLocaleDateString()}
            </span>
          </div>
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
          <ResidencyLadder
            legend="Where requests may run"
            name="residency"
            value={draft.residency}
            rungs={ORG_RUNGS}
            disabled={saving}
            showDefault
            /* The organisation has nothing to inherit from, so no `inherit`
               option is offered and the ladder can never emit null here. */
            onChange={(residency) => {
              if (residency !== null) setDraft((p) => ({ ...p, residency }))
            }}
          />

          {/* What the chosen rung costs today, counted rather than described.
              Only shown once it costs something: a line that always reads
              "nothing is refused" is a line nobody checks. */}
          {refusedByDraft.length > 0 && (
            <p className={styles.effect}>
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
                <path d="M7 11V7a5 5 0 0 1 10 0v4" />
              </svg>
              {refusedByDraft.length === 1
                ? '1 enabled model is refused by this policy.'
                : `${refusedByDraft.length} enabled models are refused by this policy.`}{' '}
              {refusedClassSummary(refusedByDraft)}
            </p>
          )}

          {/* Unreported models are refused on every rung, so this is a fact
              about the catalogue rather than about the choice above it. */}
          {unknownEnabled.length > 0 && (
            <p className={styles.effectQuiet}>
              {unknownEnabled.length === 1
                ? '1 enabled model has not reported where it runs'
                : `${unknownEnabled.length} enabled models have not reported where they run`}
              , so they are refused whichever option is chosen. They answer again as soon as a
              backend reports them.
            </p>
          )}

          {/* What a request may CARRY is no longer said here. Images and tool
              calls are refused on every backend, before a route is resolved, so
              the limit is not a consequence of this rung and reading it under
              "where requests may run" implies it is. It is stated once on the
              Models tab, and again on any route that binds the multimodal
              signal, where it is actionable. */}

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

          {/* affected counts only principals with a policy of their own whose
              allowed set actually shrank — never "has a row". orgNarrowed is
              the separate question of whether everyone who simply inherits now
              is allowed less too; the two can both be true, either alone, or
              neither. `!changed` is how the card knows the save landed: a
              failed one leaves the form ahead of the stored policy. */}
          {affected !== null && !changed && (orgNarrowed || affected > 0) && (
            <p className={styles.affected} role="status">
              {orgNarrowed && "Everyone who inherits the organisation's policy is now allowed fewer models."}
              {orgNarrowed && affected > 0 && ' '}
              {affected > 0 &&
                (affected === 1
                  ? '1 user or key with a policy of its own is also allowed fewer.'
                  : `${affected} users and keys with a policy of their own are also allowed fewer.`)}
            </p>
          )}

          {affected !== null && !changed && !orgNarrowed && affected === 0 && (
            <p className={styles.affectedQuiet} role="status">
              {unresolved.length > 0
                ? 'No one is allowed fewer of the models a backend has reported.'
                : 'No one is allowed fewer models than before.'}
            </p>
          )}

          {/* What the counts above could not judge: enabled, allowed models no
              backend has reported yet, so their class is unknown. Said on its
              own rather than folded into the count either way. */}
          {affected !== null && !changed && unresolved.length > 0 && (
            <p className={styles.affectedQuiet} role="status">
              {unresolved.length === 1
                ? `1 enabled model has not been seen by a backend yet (${modelLabel(unresolved[0], models)}); if it turns out to be cloud-managed it will also be refused.`
                : `${unresolved.length} enabled models have not been seen by a backend yet (${unresolved
                    .slice(0, 4)
                    .map((id) => modelLabel(id, models))
                    .join(', ')}${unresolved.length > 4 ? ` and ${unresolved.length - 4} more` : ''}); any that turn out to be cloud-managed will also be refused.`}
            </p>
          )}

          {/* A refusal with no structure behind it. The API's sentence names
              the rung, the route and the model it conflicts with, so it is
              shown whole rather than summarised into something vaguer. */}
          {refusal !== null && (
            <div className={styles.blocked} role="alert">
              <p className={styles.warningsTitle}>Not saved.</p>
              <p className={styles.warningsHint}>{refusal}</p>
              <p className={styles.warningsHint}>
                Repoint the route under Admin &gt; Routing, or choose a rung that permits where it
                points. Nothing has changed.
              </p>
            </div>
          )}

          {lockedOut.length > 0 && (
            <div className={styles.blocked} role="alert">
              <p className={styles.warningsTitle}>
                Not saved.{' '}
                {lockedOut.length === 1 && lockedOut[0] === '(organisation)'
                  ? 'This policy would leave the whole organisation with no model.'
                  : lockedOut.length === 1
                    ? 'This policy would leave one user or key with no model it may use:'
                    : `This policy would leave ${lockedOut.length} users or keys with no model they may use:`}
              </p>
              {!(lockedOut.length === 1 && lockedOut[0] === '(organisation)') && (
                <ul className={styles.warningsList}>
                  {lockedOut.map((name) => (
                    <li key={name}>{name}</li>
                  ))}
                </ul>
              )}
              <p className={styles.warningsHint}>
                Their own allowed lists have nothing in common with this one. Widen the
                organisation's list, or change their policies in Users &amp; Keys first — a key can
                never reach more than its owner, and nobody more than the organisation.
              </p>
            </div>
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
        open={confirmKind !== 'none'}
        onClose={() => setConfirmKind('none')}
        title={
          confirmKind === 'warnings'
            ? 'Policy saved'
            : confirmKind === 'loosen'
              ? leavingAccount
                ? 'Allow prompts to leave your cloud account?'
                : 'Widen where requests may run?'
              : 'Narrow what the organisation can reach?'
        }
        description={
          confirmKind === 'warnings'
            ? 'The policy has changed. These routes need your attention.'
            : confirmKind === 'loosen'
              ? leavingAccount
                ? "Requests will be allowed to run at a model vendor's own API, using your organisation's key."
                : 'More of what is already enabled will be allowed to answer.'
              : 'Everyone inherits this. A user or key that already has its own policy keeps it, but can never reach past the organisation.'
        }
      >
        <div className={styles.confirmBody}>
          {confirmKind === 'warnings' && (
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
                <Button type="button" onClick={() => setConfirmKind('none')}>Done</Button>
              </div>
            </>
          )}

          {/* Loosening. The body is about where prompts go, not about who gains
              what: the people reading this are answering to someone about the
              account boundary, and that is the only fact that matters here. */}
          {confirmKind === 'loosen' && (
            <>
              {leavingAccount ? (
                <>
                  <p className={styles.confirmText}>
                    Prompts, and the text of the files attached to them, are sent to a vendor over
                    the public internet and handled under that vendor's terms — not under your
                    cloud account's controls. Darkplum cannot see how the key is scoped at the
                    vendor, and there is no spend limit in this product.
                  </p>
                  {openedVendors.length > 0 ? (
                    <p className={styles.confirmText}>
                      Enabled today, and refused until now:{' '}
                      {openedByDraft.length === 1 ? '1 model' : `${openedByDraft.length} models`} at{' '}
                      {openedVendors.join(' and ')}.
                    </p>
                  ) : (
                    <p className={styles.confirmText}>
                      No vendor models are enabled yet, so nothing changes today. Anything you
                      enable afterwards is allowed by this policy without a second decision.
                    </p>
                  )}
                  {openedByDraft.length > 0 && (
                    <>
                      <ul className={styles.confirmList}>
                        {openedByDraft.slice(0, 5).map((m) => (
                          <li key={m.id}>
                            <code className={styles.confirmModel}>{m.model_id}</code>
                          </li>
                        ))}
                      </ul>
                      {openedByDraft.length > 5 && (
                        <p className={styles.confirmMore}>and {openedByDraft.length - 5} more</p>
                      )}
                    </>
                  )}
                  {/* This dialog lists what changes by allowing prompts out of
                      the account. What a request may carry does not change:
                      images and tool calls are refused on every backend either
                      way, so saying it here read as a vendor limit. */}
                </>
              ) : (
                <p className={styles.confirmText}>
                  {openedByDraft.length === 0
                    ? 'Nothing enabled today is affected. The policy still applies to anything you enable later.'
                    : `${openedByDraft.length === 1 ? '1 enabled model starts' : `${openedByDraft.length} enabled models start`} answering again. They all run inside your cloud account.`}
                </p>
              )}
              <p className={styles.confirmText}>{POLICY_TIMING}</p>
              <div className={styles.confirmFooter}>
                <Button type="button" variant="ghost" onClick={() => setConfirmKind('none')}>
                  Cancel
                </Button>
                <Button
                  type="button"
                  variant={leavingAccount ? 'danger' : 'primary'}
                  onClick={() => void commit()}
                  disabled={saving}
                >
                  {saving ? 'Saving…' : leavingAccount ? 'Allow external models' : 'Save policy'}
                </Button>
              </div>
            </>
          )}

          {confirmKind === 'tighten' && (
            <>
              {refusedByDraft.length > 0 && (
                <>
                  <p className={styles.confirmText}>
                    {refusedByDraft.length} enabled{' '}
                    {refusedByDraft.length === 1 ? 'model stops' : 'models stop'} answering,
                    including anyone using an alias that points at{' '}
                    {refusedByDraft.length === 1 ? 'it' : 'them'}:
                  </p>
                  <ul className={styles.confirmList}>
                    {refusedByDraft.slice(0, 5).map((m) => (
                      <li key={m.id}>
                        <code className={styles.confirmModel}>{m.model_id}</code>
                        <span className={styles.confirmClass}>
                          {backendClassLabel(displayClassOf(m))}
                        </span>
                      </li>
                    ))}
                  </ul>
                  {refusedByDraft.length > 5 && (
                    <p className={styles.confirmMore}>and {refusedByDraft.length - 5} more</p>
                  )}
                </>
              )}
              {draft.allowed_models !== null && (
                <p className={styles.confirmText}>
                  {draft.allowed_models.length} of {enabled.length} enabled models stay reachable.
                  Everything else is refused, whoever asks for it.
                </p>
              )}
              {refusedByDraft.length === 0 && draft.allowed_models === null && (
                <p className={styles.confirmText}>
                  Nothing enabled today is refused by this, so no one loses access right now. The
                  policy still applies to anything you enable later.
                </p>
              )}
              <p className={styles.confirmText}>{POLICY_TIMING}</p>
              <div className={styles.confirmFooter}>
                <Button type="button" variant="ghost" onClick={() => setConfirmKind('none')}>
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
