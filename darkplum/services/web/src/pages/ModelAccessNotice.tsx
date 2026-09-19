import { useCallback, useEffect, useState } from 'react'
import { getMyPolicyEffective } from '../lib/api'
import { POLICY_RULE, policyLimits, residencyLabel, sourceLabelForSelf } from '../lib/policy'
import { formatModelName } from '../lib/formatModelName'
import type { EffectivePolicy, Model } from '../types'
import { Button, Modal } from '../components/ui'
import styles from './ModelAccessNotice.module.css'

interface Props {
  /** The entries the picker is offering — used to name allowed ids the way the
   *  picker names them, rather than showing a raw id twice. */
  models: Model[]
  /** True while the page is already showing that the policy service is down
   *  (the unfiltered-list banner); this component then stays quiet on error. */
  suppressError?: boolean
}

function label(id: string, models: Model[]): string {
  const match = models.find((m) => m.id === id)
  if (match?.alias_of) return `${match.id} — ${formatModelName(match.alias_of)}`
  return formatModelName(id)
}

/**
 * A quiet way for someone in chat to find out why their model list is shorter
 * than a colleague's. It reads the merged policy for the signed-in person from
 * the one endpoint that computes it, so what is explained here is exactly what
 * the gateway enforces — never a second guess at the same rule.
 *
 * The affordance only appears once the answer is worth reading: when nothing is
 * narrowed, there is nothing to explain and the header stays clean.
 */
export function ModelAccessNotice({ models, suppressError = false }: Props) {
  const [open, setOpen] = useState(false)
  const [policy, setPolicy] = useState<EffectivePolicy | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setPolicy(await getMyPolicyEffective())
      setError(null)
    } catch (err) {
      // Shown below as a small error with a retry: the "why" affordance is
      // what someone reaches for when access looks wrong, so it must not
      // vanish at exactly that moment.
      setError(err instanceof Error ? err.message : 'Your model access could not be read')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  if (loading) return null

  // Read failed — the "why" affordance is exactly what someone reaches for
  // when access looks wrong, so it stays put and offers a retry instead of
  // quietly vanishing.
  if (error) {
    // When the page already says the policy service is unreachable (the
    // model-list banner), a second red box for the same outage is noise.
    if (suppressError) return null
    return (
      <div className={styles.loadError} role="alert">
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="12" cy="12" r="10" />
          <line x1="12" y1="8" x2="12" y2="13" />
          <line x1="12" y1="16" x2="12.01" y2="16" />
        </svg>
        <span>Could not load your access policy — try again</span>
        <button type="button" className={styles.retryBtn} onClick={() => void load()}>
          Retry
        </button>
      </div>
    )
  }

  if (!policy || !policyLimits(policy)) return null

  const allowedSource = policy.sources?.allowed_models
  const residencySource = policy.sources?.residency
  const principalSourced =
    allowedSource === 'user' || allowedSource === 'api_key' ||
    residencySource === 'user' || residencySource === 'api_key'
  const orgSourced = allowedSource === 'org' || residencySource === 'org'

  return (
    <>
      <button
        type="button"
        className={styles.trigger}
        onClick={() => setOpen(true)}
        aria-label="Model access is limited by policy — why?"
      >
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
        </svg>
        {/* The sentence is the affordance. On a narrow header the shield keeps
            its place and the accessible name carries the rest. */}
        <span className={styles.triggerText} aria-hidden="true">
          Model access is limited by policy — why?
        </span>
      </button>

      <Modal
        open={open}
        onClose={() => setOpen(false)}
        title="Why your model list is what it is"
        description="Your organisation decides which models can answer. This is the policy as it applies to you right now."
      >
        <div className={styles.body}>
          <dl className={styles.grid}>
            <dt>Models you can use</dt>
            <dd>
              {policy.allowed_models === null ? (
                'Every model the organisation has enabled'
              ) : policy.allowed_models.length === 0 ? (
                <span className={styles.none}>None — ask an administrator</span>
              ) : (
                <span className={styles.chips}>
                  {policy.allowed_models.map((id) => (
                    <span key={id} className={styles.chip}>{label(id, models)}</span>
                  ))}
                </span>
              )}
              {policy.sources?.allowed_models && (
                <span className={styles.source}>{sourceLabelForSelf(policy.sources.allowed_models)}</span>
              )}
            </dd>

            <dt>Where they run</dt>
            <dd>
              {policy.residency === 'self_hosted_only'
                ? 'Self-hosted only — nothing leaves your infrastructure'
                : residencyLabel(policy.residency)}
              {policy.sources?.residency && (
                <span className={styles.source}>{sourceLabelForSelf(policy.sources.residency)}</span>
              )}
            </dd>

            <dt>Starts on</dt>
            <dd>
              {policy.default_model ? label(policy.default_model, models) : 'Nothing in particular'}
              {policy.sources?.default_model && (
                <span className={styles.source}>{sourceLabelForSelf(policy.sources.default_model)}</span>
              )}
            </dd>
          </dl>

          <p className={styles.rule}>{POLICY_RULE}</p>
          <p className={styles.hint}>
            Nothing here is something you can change yourself.{' '}
            {/* Named, not linked: whoever reads this is being told their access
                is narrow, which usually means they are not the administrator
                who could follow the link. */}
            {principalSourced && orgSourced
              ? "Some of this comes from the organisation's policy, widened under Admin → Models → Policy; the rest is set on you or your key, in Admin → Users & Keys."
              : principalSourced
                ? 'An administrator can widen it from Admin → Users & Keys — this is set on you or your key, not on the organisation.'
                : 'An administrator can widen it under Admin → Models → Policy.'}
          </p>

          <div className={styles.footer}>
            <Button type="button" onClick={() => setOpen(false)}>Close</Button>
          </div>
        </div>
      </Modal>
    </>
  )
}
