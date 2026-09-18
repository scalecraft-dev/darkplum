import { useState } from 'react'
import type { AdminModel, Residency, RoutingPolicy } from '../types'
import { backendClassOf } from '../lib/modelRouting'
import { Button, Modal } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './RoutingPolicyCard.module.css'

interface Props {
  policy: RoutingPolicy | null
  loading: boolean
  error: string | null
  saving: boolean
  /** Every discovered model — used to say what a tighter policy would block. */
  models: AdminModel[]
  onRetry: () => void
  /** Resolves with the routed-alias routes the new policy will refuse. */
  onChange: (residency: Residency) => Promise<string[]>
}

const OPTIONS: { value: Residency; label: string; consequence: string }[] = [
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

export function RoutingPolicyCard({
  policy,
  loading,
  error,
  saving,
  models,
  onRetry,
  onChange,
}: Props) {
  const [confirmOpen, setConfirmOpen] = useState(false)
  /** What the API said the new policy breaks. Written by the API, shown as it
   *  came: these name routed aliases that will start refusing. */
  const [warnings, setWarnings] = useState<string[]>([])

  const residency: Residency = policy?.residency ?? 'any'
  const cloudEnabled = models.filter(
    (m) => backendClassOf(m) === 'cloud_managed' && m.enabled,
  )

  const handleSelect = (value: Residency) => {
    if (value === residency || saving) return
    if (value === 'self_hosted_only') {
      setWarnings([])
      setConfirmOpen(true)
      return
    }
    void onChange(value).then(setWarnings)
  }

  /** The switch happens, then the consequences are named. Anything the API
   *  warns about stays on screen after the dialog closes — it is work to do. */
  const handleConfirm = async () => {
    const next = await onChange('self_hosted_only')
    setWarnings(next)
    if (next.length === 0) setConfirmOpen(false)
  }

  return (
    <section
      className={`${styles.card} ${residency === 'self_hosted_only' ? styles.cardRestricted : ''}`}
      aria-labelledby="routing-policy-heading"
    >
      <div className={styles.head}>
        <div>
          <h2 id="routing-policy-heading" className={styles.title}>Routing policy</h2>
          <p className={styles.subtitle}>Where chat requests are allowed to run.</p>
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
          <div className={styles.options} role="radiogroup" aria-labelledby="routing-policy-heading">
            {OPTIONS.map((opt) => {
              const checked = residency === opt.value
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
                      onChange={() => handleSelect(opt.value)}
                    />
                    <span className={styles.optionLabel}>{opt.label}</span>
                  </span>
                  <span className={styles.optionConsequence}>{opt.consequence}</span>
                </label>
              )
            })}
          </div>

          {residency === 'self_hosted_only' && (
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
        </>
      )}

      <Modal
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        title={warnings.length > 0 ? 'Now self-hosted only' : 'Switch to self-hosted only?'}
        description={
          warnings.length > 0
            ? 'The policy has changed. These routes need your attention.'
            : 'Requests to cloud-managed models such as AWS Bedrock will be refused with a policy error, even when the model is enabled.'
        }
      >
        <div className={styles.confirmBody}>
          {warnings.length > 0 ? (
            <>
              <p className={styles.confirmText}>
                The policy is now self-hosted only.{' '}
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
          ) : cloudEnabled.length === 0 ? (
            <p className={styles.confirmText}>
              No cloud-managed models are enabled today, so no one loses access right now. The
              policy still applies to anything you enable later.
            </p>
          ) : (
            <>
              <p className={styles.confirmText}>
                {cloudEnabled.length} enabled {cloudEnabled.length === 1 ? 'model' : 'models'} stop
                answering, including anyone using an alias that points at {cloudEnabled.length === 1 ? 'it' : 'them'}:
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
          {warnings.length === 0 && (
            <div className={styles.confirmFooter}>
              <Button type="button" variant="ghost" onClick={() => setConfirmOpen(false)}>
                Cancel
              </Button>
              <Button type="button" onClick={() => void handleConfirm()} disabled={saving}>
                {saving ? 'Switching...' : 'Switch to self-hosted only'}
              </Button>
            </div>
          )}
        </div>
      </Modal>
    </section>
  )
}
