import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, installBillingKey, removeBillingKey, verifyBillingKey } from '../lib/api'
import {
  BILLING_KEY_NAME,
  BILLING_KEY_WHERE,
  BILLING_TRANSPORT_HELP,
  billingVerifyErrorCopy,
} from '../lib/billing'
import { formatRetryCountdown, vendorLabel } from '../lib/modelRouting'
import { WEB_PORT_FORWARD_COMMAND, keyFingerprint, vendorKeyProblem } from '../lib/vendorKeys'
import type { VendorName } from '../types'
import { Button, CodeBlock, Input, Modal } from '../components/ui'
import styles from './VendorInstallModal.module.css'

/**
 * Installing, replacing or removing one vendor's BILLING key.
 *
 * A sibling of VendorInstallModal, not a fifth mode of it: the owner's call is
 * that this credential is "a clearly separate thing", and one dialog serving
 * both would put an inference key and an admin key one radio button apart.
 * It reuses that modal's stylesheet so the two read as one family.
 *
 * The secret rules are the inference key's, unchanged: an uncontrolled
 * password input read by ref, cleared in every exit (success, failure, close,
 * unmount), never in state, never echoed, no show/hide toggle, and only a
 * length-and-last-four line said about it.
 */

export type BillingKeyMode = 'install' | 'replace' | 'remove'

type Step = 'entry' | 'checking' | 'checked' | 'saving'

interface Props {
  open: boolean
  vendor: VendorName
  mode: BillingKeyMode
  /** False when the api would refuse a key over this page's transport. */
  transportOk: boolean
  onClose: () => void
  onDone: (kind: 'installed' | 'removed') => void
}

/** The verify sample's bucket count, whatever shape the vendor's report took. */
function bucketCount(buckets: unknown): number | null {
  if (typeof buckets === 'number' && Number.isFinite(buckets)) return buckets
  if (Array.isArray(buckets)) return buckets.length
  return null
}

export function VendorBillingKeyModal({ open, vendor, mode, transportOk, onClose, onDone }: Props) {
  const name = vendorLabel(vendor)
  const keyName = BILLING_KEY_NAME[vendor]
  const needsKey = mode !== 'remove'

  const keyRef = useRef<HTMLInputElement>(null)
  const [fingerprint, setFingerprint] = useState<{ text: string; spoken: string } | null>(null)
  const [step, setStep] = useState<Step>('entry')
  const [sample, setSample] = useState<{ day: string; buckets: number | null } | null>(null)
  const [keyError, setKeyError] = useState<string | null>(null)
  const [modalError, setModalError] = useState<string | null>(null)
  const [refusedTransport, setRefusedTransport] = useState(false)
  const [retrySeconds, setRetrySeconds] = useState(0)
  const [confirmText, setConfirmText] = useState('')
  const [askDiscard, setAskDiscard] = useState(false)
  const [clearedAfterFailure, setClearedAfterFailure] = useState(false)
  const [announcement, setAnnouncement] = useState('')

  const clearKey = useCallback(() => {
    if (keyRef.current) keyRef.current.value = ''
    setFingerprint(null)
  }, [])

  const close = useCallback(() => {
    clearKey()
    setAskDiscard(false)
    onClose()
  }, [clearKey, onClose])

  useEffect(() => clearKey, [clearKey])

  useEffect(() => {
    if (!open) return
    clearKey()
    setStep('entry')
    setSample(null)
    setKeyError(null)
    setModalError(null)
    setRefusedTransport(false)
    setRetrySeconds(0)
    setConfirmText('')
    setAskDiscard(false)
    setClearedAfterFailure(false)
    setAnnouncement('')
  }, [open, mode, vendor, clearKey])

  useEffect(() => {
    if (retrySeconds <= 0) return
    const id = setInterval(() => setRetrySeconds((s) => (s <= 1 ? 0 : s - 1)), 1000)
    return () => clearInterval(id)
  }, [retrySeconds])

  /** Puts one failure where it belongs: about the key under the key field,
   *  about everything else in the modal's error line. */
  function applyError(err: unknown): void {
    setKeyError(null)
    setModalError(null)
    if (!(err instanceof ApiError)) {
      const msg = err instanceof Error ? err.message : 'The request could not be completed.'
      setModalError(msg)
      setAnnouncement(msg)
      return
    }
    if (err.status === 429) {
      setRetrySeconds(err.retryAfterSeconds ?? 0)
      setModalError(err.message)
      setAnnouncement('Too many attempts. The button comes back shortly.')
      return
    }
    const code = err.code()
    if (code === 'insecure_transport') {
      setRefusedTransport(true)
      setModalError(err.message)
      clearKey()
      // What is on screen is the notice, not the api's sentence.
      setAnnouncement(BILLING_TRANSPORT_HELP)
      return
    }
    const copy = billingVerifyErrorCopy(vendor, code, err.message, err.vendorError()?.status ?? null)
    if (code === 'auth_failed' || code === 'wrong_key_type' || code === 'vendor_error') setKeyError(copy)
    else setModalError(copy)
    setAnnouncement(copy)
  }

  const check = async () => {
    const raw = keyRef.current?.value ?? ''
    const problem = vendorKeyProblem(raw)
    if (problem) {
      setKeyError(problem)
      setAnnouncement(problem)
      keyRef.current?.focus()
      return
    }
    setKeyError(null)
    setModalError(null)
    setClearedAfterFailure(false)
    setStep('checking')
    try {
      const result = await verifyBillingKey(vendor, raw.trim())
      setSample({ day: result.sample?.day ?? '', buckets: bucketCount(result.sample?.buckets) })
      setStep('checked')
      setAnnouncement(`Key accepted. ${name}'s usage report answered.`)
    } catch (err) {
      applyError(err)
      setStep('entry')
    }
  }

  const install = async () => {
    const raw = keyRef.current?.value ?? ''
    if (raw.trim() === '') {
      setStep('entry')
      setKeyError('The key field is empty. Paste the key again.')
      return
    }
    setStep('saving')
    setModalError(null)
    setAnnouncement('Storing the billing key.')
    try {
      await installBillingKey(vendor, raw.trim())
      onDone('installed')
      close()
    } catch (err) {
      applyError(err)
      setClearedAfterFailure(true)
      setSample(null)
      setStep('entry')
    } finally {
      clearKey()
    }
  }

  const remove = async () => {
    setStep('saving')
    setModalError(null)
    setAnnouncement(`Removing the ${name} billing key.`)
    try {
      await removeBillingKey(vendor)
      onDone('removed')
      close()
    } catch (err) {
      applyError(err)
      setStep('entry')
    }
  }

  const title =
    mode === 'install'
      ? `Install a ${name} billing key`
      : mode === 'replace'
        ? `Replace the ${name} billing key`
        : `Remove the ${name} billing key`

  const description =
    mode === 'remove'
      ? `Darkplum stops pulling ${name}'s usage and cost reports. Rows already reconciled stay.`
      : `For reconciliation only. This is ${name}'s ${keyName}, not the inference key: the api holds it in its own Secret and one daily job uses it. The gateway never sees it.`

  const busy = step === 'checking' || step === 'saving'
  const keyBlocked = needsKey && (!transportOk || refusedTransport)
  const keyTyped = fingerprint !== null
  const dismissible = !busy && !keyTyped
  const confirmMatches = confirmText.trim().toLowerCase() === vendor

  return (
    <Modal
      open={open}
      onClose={close}
      title={title}
      description={description}
      dismissible={dismissible}
      onDismissAttempt={() => {
        if (busy) {
          setAnnouncement('Waiting for the server. This finishes on its own.')
          return
        }
        setAskDiscard(true)
      }}
    >
      <div className={styles.body}>
        <p className={styles.srOnly} role="status">{announcement}</p>

        {askDiscard && (
          <div className={styles.discard} role="alert">
            <p className={styles.discardText}>Discard the key you typed?</p>
            <div className={styles.discardActions}>
              <Button type="button" size="sm" variant="ghost" onClick={() => setAskDiscard(false)}>
                Keep typing
              </Button>
              <Button type="button" size="sm" variant="danger" onClick={close}>
                Discard
              </Button>
            </div>
          </div>
        )}

        {keyBlocked && (
          <div className={styles.notice}>
            <p className={styles.noticeText}>{BILLING_TRANSPORT_HELP}</p>
            <CodeBlock>
              <code>{WEB_PORT_FORWARD_COMMAND}</code>
            </CodeBlock>
          </div>
        )}

        {/* Not while the transport notice is up: it already says the remedy. */}
        {modalError && !keyBlocked && <p className={styles.error} role="alert">{modalError}</p>}

        {needsKey && !keyBlocked && (
          <>
            {/* The scope warning, before the field rather than after it: the
                one thing to know about this credential is how much it can do. */}
            <div className={styles.boundary}>
              <p className={styles.boundaryText}>
                An {keyName} can list your organisation's workspaces, projects and keys. Darkplum uses
                it only to read usage and cost reports, but it can do more than that — treat it like
                the admin credential it is.
              </p>
            </div>
            <p className={styles.hint}>{BILLING_KEY_WHERE[vendor]}</p>
            <Input
              ref={keyRef}
              type="password"
              label={mode === 'replace' ? `New ${name} ${keyName}` : `${name} ${keyName}`}
              autoComplete="new-password"
              name="vendor-billing-key-entry"
              spellCheck={false}
              autoCorrect="off"
              autoCapitalize="off"
              data-1p-ignore=""
              data-lpignore="true"
              disabled={busy || step === 'checked'}
              error={keyError ?? undefined}
              aria-describedby={`${vendor}-billing-key-shape`}
              onChange={(e) => {
                setFingerprint(keyFingerprint(e.currentTarget.value))
                setClearedAfterFailure(false)
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  if (step === 'entry') void check()
                }
              }}
            />
            <p className={styles.shape} aria-hidden="true">
              {fingerprint?.text ?? 'Nothing pasted yet'}
            </p>
            <span className={styles.srOnly} id={`${vendor}-billing-key-shape`}>
              {fingerprint?.spoken ?? 'Nothing pasted yet'}
            </span>
            {clearedAfterFailure && (
              <p className={styles.hint}>
                The field was emptied when the attempt failed. Paste the key again to retry.
              </p>
            )}
            {(step === 'entry' || step === 'checking') && (
              <p className={styles.hint}>
                The key is checked by reading one day of {name}'s usage report before anything is
                stored. It is never shown again.
              </p>
            )}
            {step === 'checking' && <p className={styles.text}>Checking with {name}…</p>}
            {(step === 'checked' || step === 'saving') && sample && (
              <p className={styles.text} role="status">
                Key accepted. {name}'s report for{' '}
                <code className={styles.itemId}>{sample.day || 'the last day'}</code> answered
                {sample.buckets !== null
                  ? ` with ${sample.buckets.toLocaleString()} ${sample.buckets === 1 ? 'entry' : 'entries'}.`
                  : '.'}
              </p>
            )}
          </>
        )}

        {mode === 'remove' && (
          <>
            <p className={styles.text}>
              Inference is not affected: the {name} inference key and its models carry on as they are.
              Spend for {name} goes back to Darkplum's estimate only.
            </p>
            <p className={styles.text}>
              Removing it here does not invalidate it — revoke the {keyName} in {name}'s console
              afterwards.
            </p>
            <p className={styles.text}>
              Type <code className={styles.itemId}>{vendor}</code> to confirm.
            </p>
            <Input
              label="Vendor name"
              value={confirmText}
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="off"
              spellCheck={false}
              disabled={busy}
              onChange={(e) => setConfirmText(e.target.value)}
            />
          </>
        )}

        <div className={styles.footer}>
          {keyBlocked ? (
            <Button type="button" onClick={close}>Done</Button>
          ) : (
            <>
              <Button type="button" variant="ghost" onClick={close} disabled={busy}>
                Cancel
              </Button>

              {needsKey && (step === 'entry' || step === 'checking') && (
                <Button type="button" onClick={() => void check()} disabled={busy || retrySeconds > 0}>
                  {step === 'checking'
                    ? 'Checking…'
                    : retrySeconds > 0
                      ? `Try again in ${formatRetryCountdown(retrySeconds)}`
                      : 'Check key'}
                </Button>
              )}

              {needsKey && step === 'checked' && (
                <Button
                  type="button"
                  variant="ghost"
                  onClick={() => {
                    setSample(null)
                    setStep('entry')
                    setAnnouncement('Editing the key. Check it again before installing.')
                  }}
                >
                  Use a different key
                </Button>
              )}

              {needsKey && (step === 'checked' || step === 'saving') && (
                <Button
                  type="button"
                  variant="danger"
                  onClick={() => void install()}
                  disabled={busy || retrySeconds > 0}
                >
                  {step === 'saving'
                    ? 'Storing…'
                    : retrySeconds > 0
                      ? `Try again in ${formatRetryCountdown(retrySeconds)}`
                      : mode === 'replace'
                        ? 'Replace billing key'
                        : 'Install billing key'}
                </Button>
              )}

              {mode === 'remove' && (
                <Button
                  type="button"
                  variant="danger"
                  onClick={() => void remove()}
                  disabled={busy || !confirmMatches}
                >
                  {step === 'saving' ? 'Removing…' : 'Remove billing key'}
                </Button>
              )}
            </>
          )}
        </div>
      </div>
    </Modal>
  )
}
