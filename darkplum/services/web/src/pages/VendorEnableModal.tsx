import { useEffect, useMemo, useState } from 'react'
import type { AdminModel, Residency, VendorName } from '../types'
import { vendorLabel } from '../lib/modelRouting'
import { POLICY_TIMING, residencyLabel, residencyPermits } from '../lib/policy'
import { formatModelName } from '../lib/formatModelName'
import { Button, Modal } from '../components/ui'
import styles from './VendorEnableModal.module.css'

/**
 * Turning a vendor on, in one decision.
 *
 * Two things have to be true before a vendor model can answer: the routing
 * policy has to permit models outside the cloud account, and the models
 * themselves have to be enabled. Making those two separate errands is how an
 * administrator ends up with a catalogue full of switched-on models that every
 * request refuses, and no idea which of the two steps they missed.
 *
 * So this is one modal and one confirmation. It states the boundary being
 * crossed first — that is the decision; the checkboxes are the details — and
 * it never links to another tab, because a link inside a modal is a way out of
 * a decision that has not been made yet.
 */

/** Where the run got to. Kept as data rather than as three booleans so a
 *  partial failure can be reported for exactly what failed. */
interface RunResult {
  /** Set when the policy write was attempted and failed. The enables are not
   *  attempted after it: enabling models the policy still refuses produces the
   *  exact confusing state this modal exists to prevent. */
  policyError: string | null
  enabled: string[]
  failed: { id: string; error: string }[]
}

interface Props {
  open: boolean
  vendor: VendorName
  /** Every catalogue row for this vendor. */
  models: AdminModel[]
  /** The organisation's policy as stored, all three fields — the write below
   *  is a PUT, so it must carry the fields it is not changing. */
  policy: { residency: Residency; allowed_models: string[] | null; default_model: string | null }
  onClose: () => void
  /** Writes the organisation's policy. Rejects with the API's own message. */
  onSavePolicy: (residency: Residency) => Promise<void>
  /** Enables one model. Rejects with the API's own message. */
  onEnable: (model: AdminModel) => Promise<void>
  /** Everything landed — the page reloads what it needs to. */
  onDone: () => void
}

export function VendorEnableModal({
  open,
  vendor,
  models,
  policy,
  onClose,
  onSavePolicy,
  onEnable,
  onDone,
}: Props) {
  const candidates = useMemo(() => models.filter((m) => !m.enabled), [models])
  const [ticked, setTicked] = useState<Set<string>>(new Set())
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<RunResult | null>(null)

  /**
   * Whether the policy has to move, and what it says today — both frozen at
   * the moment the modal opened.
   *
   * The run writes the policy first, and the page above updates as soon as it
   * lands. Reading these live would rewrite the title and the danger button
   * out from under someone who is watching "Working…", and would leave the
   * report afterwards describing a change it no longer thinks was needed.
   */
  const [opening, setOpening] = useState({
    needsPolicy: !residencyPermits(policy.residency, 'external_api'),
    residency: policy.residency,
  })

  /** Everything is pre-ticked. Someone who opened "Review and enable" has said
   *  what they want; the list is here to be trimmed, not to be filled in. */
  useEffect(() => {
    if (!open) return
    setTicked(new Set(candidates.map((m) => m.id)))
    setResult(null)
    setRunning(false)
    setOpening({
      needsPolicy: !residencyPermits(policy.residency, 'external_api'),
      residency: policy.residency,
    })
    // The snapshot is taken when the modal opens, deliberately — re-taking it
    // as the policy changes is the bug this exists to prevent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, candidates])

  const needsPolicy = opening.needsPolicy
  const chosen = candidates.filter((m) => ticked.has(m.id))
  const vendorName = vendorLabel(vendor)

  const run = async () => {
    setRunning(true)
    const outcome: RunResult = { policyError: null, enabled: [], failed: [] }
    // The policy first, always. The other order leaves a window in which the
    // models are on and every request to them is refused.
    if (needsPolicy) {
      try {
        await onSavePolicy('any')
      } catch (err) {
        outcome.policyError = err instanceof Error ? err.message : 'The policy could not be saved'
        setResult(outcome)
        setRunning(false)
        return
      }
    }
    for (const model of chosen) {
      try {
        await onEnable(model)
        outcome.enabled.push(model.model_id)
      } catch (err) {
        outcome.failed.push({
          id: model.model_id,
          error: err instanceof Error ? err.message : 'Could not be enabled',
        })
      }
    }
    setResult(outcome)
    setRunning(false)
    onDone()
  }

  const finished = result !== null
  const allFine = finished && result.policyError === null && result.failed.length === 0

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={
        finished
          ? allFine
            ? `${vendorName} is on`
            : 'Partly done'
          : needsPolicy
            ? 'Allow prompts to leave your cloud account?'
            : `Enable ${vendorName} models`
      }
      description={
        finished
          ? undefined
          : needsPolicy
            ? `Your routing policy is set to "${residencyLabel(opening.residency)}", which refuses vendor APIs. Turning these models on means changing it.`
            : `Your routing policy already permits vendor APIs. These models start answering as soon as they are on.`
      }
    >
      <div className={styles.body}>
        {!finished && (
          <>
            {needsPolicy && (
              <div className={styles.boundary}>
                <p className={styles.boundaryText}>
                  Prompts, and the text of any files attached to them, are sent to {vendorName} over
                  the public internet and handled under their terms — not under your cloud
                  account's controls. Darkplum cannot see how the key is scoped at {vendorName}, and
                  there is no spend limit in this product: use a project-scoped key with a limit set
                  on {vendorName}'s side.
                </p>
                {/* What a request may carry is not part of this decision: tool
                    calls and images are refused on every backend, not only at
                    a vendor, so stating it here made a product-wide limit read
                    as the price of turning this vendor on. It is on the Models
                    tab behind this modal, and on any route that binds the
                    multimodal signal. */}
                <p className={styles.boundaryChange}>
                  Where requests may run changes from "{residencyLabel(opening.residency)}" to "
                  {residencyLabel('any')}". That applies to the whole organisation, not only to
                  these models.
                </p>
              </div>
            )}

            {candidates.length === 0 ? (
              <p className={styles.text}>
                Every {vendorName} model in the catalogue is already enabled.
                {needsPolicy && ' They start answering once the policy above changes.'}
              </p>
            ) : (
              <fieldset className={styles.picker}>
                <legend className={styles.legend}>
                  Models to enable — {chosen.length} of {candidates.length} ticked
                </legend>
                <ul className={styles.list}>
                  {candidates.map((m) => (
                    <li key={m.id}>
                      <label className={styles.item}>
                        <input
                          type="checkbox"
                          className={styles.checkbox}
                          checked={ticked.has(m.id)}
                          disabled={running}
                          onChange={() =>
                            setTicked((prev) => {
                              const next = new Set(prev)
                              if (next.has(m.id)) next.delete(m.id)
                              else next.add(m.id)
                              return next
                            })
                          }
                        />
                        <span className={styles.itemText}>
                          <span className={styles.itemName}>
                            {m.display_name ?? formatModelName(m.model_id)}
                          </span>
                          <code className={styles.itemId}>{m.model_id}</code>
                        </span>
                      </label>
                    </li>
                  ))}
                </ul>
              </fieldset>
            )}

            {/* An allowlist is a second gate this modal deliberately does not
                open: widening it is a policy decision of its own, and doing it
                silently here would be the opposite of what an allowlist is
                for. Said plainly so the enable does not look broken. */}
            {policy.allowed_models !== null && chosen.length > 0 && (
              <p className={styles.caveat}>
                The organisation also has an allowed list. Models enabled here are still refused
                until they are added to it on the Policy tab.
              </p>
            )}

            <p className={styles.timing}>{POLICY_TIMING}</p>

            <div className={styles.footer}>
              <Button type="button" variant="ghost" onClick={onClose} disabled={running}>
                Cancel
              </Button>
              <Button
                type="button"
                variant={needsPolicy ? 'danger' : 'primary'}
                onClick={() => void run()}
                disabled={running || (chosen.length === 0 && !needsPolicy)}
              >
                {running
                  ? 'Working…'
                  : needsPolicy
                    ? 'Allow external models'
                    : 'Enable models'}
              </Button>
            </div>
          </>
        )}

        {/* The report stays in the modal that caused it. A toast for a run
            that half-succeeded is gone before it can be read, and the list of
            what failed is the only way to know what to try again. */}
        {finished && (
          <div role="status">
            {result.policyError !== null ? (
              <>
                <p className={styles.failTitle}>The routing policy was not changed.</p>
                <p className={styles.failDetail}>{result.policyError}</p>
                <p className={styles.text}>
                  Nothing was enabled. Turning these models on while the policy still refuses them
                  would leave the catalogue looking ready and every request refused.
                </p>
              </>
            ) : (
              <>
                {needsPolicy && (
                  <p className={styles.text}>
                    Where requests may run is now "{residencyLabel('any')}".
                  </p>
                )}
                <p className={styles.text}>
                  {result.enabled.length === 0
                    ? 'No models were enabled.'
                    : result.enabled.length === 1
                      ? '1 model enabled.'
                      : `${result.enabled.length} models enabled.`}
                </p>
                {result.failed.length > 0 && (
                  <>
                    <p className={styles.failTitle}>
                      {result.failed.length === 1
                        ? '1 model could not be enabled:'
                        : `${result.failed.length} models could not be enabled:`}
                    </p>
                    <ul className={styles.failList}>
                      {result.failed.map((f) => (
                        <li key={f.id}>
                          <code className={styles.itemId}>{f.id}</code>
                          <span className={styles.failDetail}>{f.error}</span>
                        </li>
                      ))}
                    </ul>
                    <p className={styles.text}>
                      The rest were enabled. Use the switch on each row to try these again.
                    </p>
                  </>
                )}
              </>
            )}
            <div className={styles.footer}>
              <Button type="button" onClick={onClose}>Done</Button>
            </div>
          </div>
        )}
      </div>
    </Modal>
  )
}
