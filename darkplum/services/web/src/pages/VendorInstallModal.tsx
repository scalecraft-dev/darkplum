import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ApiError,
  installVendorKey,
  removeVendorKey,
  setVendorFilter,
  verifyVendorKey,
  type VendorVerification,
} from '../lib/api'
import { formatModelName } from '../lib/formatModelName'
import { VENDOR_KEY_COMMAND, formatRetryCountdown, vendorLabel } from '../lib/modelRouting'
import {
  KEY_TRANSPORT_HELP_TEXT,
  WEB_PORT_FORWARD_COMMAND,
  admittedDiff,
  groupAdmittedModels,
  keyFingerprint,
  seedTickedModels,
  serialiseModelFilter,
  vendorErrorCopy,
  vendorKeyProblem,
  vendorTakesWorkspaceId,
  workspaceIdProblem,
  workspaceIdToSend,
  workspaceRequiredCopy,
  type AdmittedDiff,
  type KeyTransportHelp,
} from '../lib/vendorKeys'
import type { VendorName } from '../types'
import { Button, CodeBlock, Input, Modal } from '../components/ui'
import styles from './VendorInstallModal.module.css'

/**
 * Installing, rotating, re-scoping or removing one vendor's key.
 *
 * Four errands, one modal, because they are one decision seen from four
 * moments: which vendor Darkplum can call, and which of that vendor's models
 * it offers. Splitting them into four dialogs would mean four places to keep
 * the same warning, the same checklist and the same rules about a secret.
 *
 * The key never leaves this file. It is read from an uncontrolled input by
 * ref, passed straight to the api function, and the field is cleared in the
 * submit's finally, on close and on unmount. It is never held in React state,
 * never a prop, never in a toast, never in an error message, and never in the
 * URL — the routes take the vendor in the path and the key in the body, so it
 * cannot reach an access log or a Referer header either.
 *
 * There is no show/hide toggle. Revealing a key puts it on a screen that may
 * be shared, and the only question anyone actually has of a pasted key —
 * "did the whole thing arrive?" — is answered by the derived line beneath the
 * field, which counts the characters and shows the last four.
 */

export type VendorInstallMode = 'install' | 'replace' | 'change-models' | 'remove'

interface Props {
  open: boolean
  vendor: VendorName
  mode: VendorInstallMode
  /** This vendor's model ids as the catalogue holds them now: what Change
   *  models starts from, and what Remove may have to turn off. */
  currentModelIds: string[]
  /** The patterns in force for this vendor. Globs as often as ids, which is
   *  why the checklist resolves them against the reachable list rather than
   *  comparing them to it. */
  currentFilter: string[]
  /** Of those, the ones that are enabled. Named in the remove flow, which the
   *  api refuses while any of them are on. */
  enabledModelIds: string[]
  /** The Anthropic workspace id stored now, or null. Prefills the workspace
   *  field; read once per opening so a poll cannot overwrite what is typed. */
  currentWorkspaceId: string | null
  /** False when the api would refuse a key over this page's transport (plain
   *  HTTP off loopback). The key form is not offered at all in that case — a
   *  disabled field would still read as "type it here". */
  transportOk: boolean
  /** Which of the two transport faults it is, so the notice names the half
   *  that is actually misconfigured. Null when a key can be installed from
   *  here — which is also the state a mid-flow 403 contradicts; see where it
   *  is read. */
  transportHelp: KeyTransportHelp | null
  onClose: () => void
  /** Something landed. The page reloads the catalogue and the vendor block;
   *  the card, not this modal, owns what happens next. */
  onDone: (outcome: Outcome) => void
}

export type Outcome =
  | { kind: 'installed'; admitted: string[]; installingUntil: string; warnings: string[] }
  | { kind: 'filter'; admitted: string[] }
  /** The api's own warnings, carried out of the modal so the card can show
   *  them beside "Removing" — they say whether the models went off with the
   *  key or are still on and about to start failing. */
  | { kind: 'removed'; warnings: string[] }

/** Where the modal is. `checking` and `saving` are apart from `busy` because
 *  they say different things on screen and disarm different controls. */
type Step = 'entry' | 'checking' | 'checked' | 'saving' | 'result'

export function VendorInstallModal({
  open,
  vendor,
  mode,
  currentModelIds,
  currentFilter,
  enabledModelIds,
  currentWorkspaceId,
  transportOk,
  transportHelp,
  onClose,
  onDone,
}: Props) {
  const vendorName = vendorLabel(vendor)
  const needsKey = mode === 'install' || mode === 'replace'
  /** Anthropic's workspace field, on the three modes that talk to the vendor
   *  with a key. Remove sends nothing to the vendor, so it is not asked. */
  const takesWorkspace = vendorTakesWorkspaceId(vendor) && mode !== 'remove'

  /** The workspace id. A controlled input like any other: it is a name, not a
   *  secret, and it is shown on the card once stored. */
  const [workspaceId, setWorkspaceId] = useState(currentWorkspaceId ?? '')
  const workspaceRef = useRef<HTMLInputElement>(null)
  const [workspaceError, setWorkspaceError] = useState<string | null>(null)
  /** Focus belongs on the workspace field after a `workspace_required`, but the
   *  field is disabled while the request that earned it is in flight. Asked
   *  for here and honoured once the field is enabled again. */
  const [focusWorkspace, setFocusWorkspace] = useState(false)

  /** The key field. Uncontrolled and read by ref, so the value has one home
   *  and clearing it is one assignment rather than a state update that leaves
   *  the old string in a fibre somewhere. */
  const keyRef = useRef<HTMLInputElement>(null)
  /** What the field says about itself: a character count and four characters.
   *  Derived on input; the key itself is never put in state. */
  const [fingerprint, setFingerprint] = useState<{ text: string; spoken: string } | null>(null)

  // Change models opens straight into a check against the installed key; the
  // other three open on something to read or type.
  const [step, setStep] = useState<Step>(mode === 'change-models' ? 'checking' : 'entry')
  const [verification, setVerification] = useState<VendorVerification | null>(null)
  const [ticked, setTicked] = useState<Set<string>>(new Set())

  /** Errors, placed where the thing that caused them is. A message under the
   *  wrong control is a message someone reads as being about that control. */
  const [keyError, setKeyError] = useState<string | null>(null)
  const [filterError, setFilterError] = useState<string | null>(null)
  const [modalError, setModalError] = useState<string | null>(null)
  /** The gateway behind this api cannot install keys. Not an error to recover
   *  from: it is a different path, and the command is shown. */
  const [oldGateway, setOldGateway] = useState(false)
  /** The api refused a key over this transport mid-flow — the page was opened
   *  over HTTPS and is not any more, or the models response predates the flag.
   *  Same shape of answer as oldGateway: the form goes away and the command
   *  takes its place, with the api's own sentence above it. */
  const [refusedTransport, setRefusedTransport] = useState(false)
  /** The key was stored and the model selection was not. Its own state, because
   *  the recovery is a different errand from the one that failed. */
  const [partial, setPartial] = useState<string | null>(null)
  /** Seconds left on a 429, counted down on the button that was refused. */
  const [retrySeconds, setRetrySeconds] = useState(0)

  const [confirmText, setConfirmText] = useState('')
  /** The models a `models_enabled` 409 named. Kept in the modal: the answer to
   *  it is a second press of the same button, not a trip to another tab. */
  const [blockingModels, setBlockingModels] = useState<string[] | null>(null)

  const [diff, setDiff] = useState<AdmittedDiff | null>(null)

  /** Asked when Escape or the overlay is used while a key is in the field. */
  const [askDiscard, setAskDiscard] = useState(false)
  /** An install was attempted and did not land. The field was emptied on the
   *  way out — see install() — so the retry starts from a paste, and saying so
   *  is the difference between "try again" and a field that looks broken. */
  const [clearedAfterFailure, setClearedAfterFailure] = useState(false)

  /** One polite live region for the whole modal, updated when the modal moves
   *  from one state to another and at no other time. A region that follows
   *  every keystroke is a region nobody can listen to. */
  const [announcement, setAnnouncement] = useState('')

  /** Empties the key field. Called in every path that leaves this modal, and
   *  on unmount, so the value never outlives the dialog that asked for it. */
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

  /** Verification with the installed key, for the two modes that do not ask
   *  for one. Its own function so "Try again" is the same call. */
  const verifyInstalled = useCallback(async (workspace: string) => {
    setStep('checking')
    setModalError(null)
    setOldGateway(false)
    try {
      const ws = workspaceIdToSend(vendor, workspace, currentWorkspaceId)
      const result = await verifyVendorKey(vendor, ws === undefined ? {} : { workspace_id: ws })
      setVerification(result)
      // Start from what is on offer today, so the checklist opens on the
      // current answer and a change is something someone did deliberately.
      setTicked((prev) =>
        seedTickedModels({
          admitted: result.admitted,
          reachable: result.reachable,
          currentFilter,
          currentModelIds,
          previous: prev,
        }),
      )
      setStep('checked')
      setAnnouncement(
        `${result.admitted.length} ${result.admitted.length === 1 ? 'model' : 'models'} available from ${vendorName}.`,
      )
    } catch (err) {
      applyError(err, 'filter')
      setStep('entry')
    }
    // applyError is stable for the life of the modal; listing it would mean
    // rebuilding this callback on every error state change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [vendor, vendorName, currentModelIds, currentFilter, currentWorkspaceId])

  // Opening resets everything. A modal that reopens holding the last run's
  // error — or the last run's ticks — is describing a request nobody made.
  useEffect(() => {
    if (!open) return
    clearKey()
    setVerification(null)
    setTicked(new Set())
    setKeyError(null)
    setFilterError(null)
    setModalError(null)
    setWorkspaceId(currentWorkspaceId ?? '')
    setWorkspaceError(null)
    setFocusWorkspace(false)
    setOldGateway(false)
    setRefusedTransport(false)
    setPartial(null)
    setRetrySeconds(0)
    setConfirmText('')
    setBlockingModels(null)
    setDiff(null)
    setAskDiscard(false)
    setClearedAfterFailure(false)
    setAnnouncement('')
    if (mode === 'change-models') void verifyInstalled(currentWorkspaceId ?? '')
    else setStep('entry')
    // verifyInstalled changes with the catalogue, which must not restart a
    // check that is already running.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, mode, vendor])

  useEffect(() => {
    if (!focusWorkspace || step !== 'entry') return
    workspaceRef.current?.focus()
    setFocusWorkspace(false)
  }, [focusWorkspace, step])

  // The 429 countdown. One interval, cleared when it reaches zero or the modal
  // goes away, so a refused submit re-arms itself rather than staying dead.
  useEffect(() => {
    if (retrySeconds <= 0) return
    const id = setInterval(() => setRetrySeconds((s) => (s <= 1 ? 0 : s - 1)), 1000)
    return () => clearInterval(id)
  }, [retrySeconds])

  /**
   * Puts one failure where it belongs.
   *
   * `where` is which control the caller was operating, because the same code
   * means different things in different places: filter_matched_nothing under
   * the key field would read as a problem with the key.
   */
  function applyError(err: unknown, where: 'key' | 'filter' | 'remove'): void {
    setKeyError(null)
    setFilterError(null)
    setModalError(null)
    setWorkspaceError(null)
    if (!(err instanceof ApiError)) {
      setModalError(err instanceof Error ? err.message : 'The request could not be completed.')
      setAnnouncement('The request could not be completed.')
      return
    }
    // The server's own sentence, and nothing of what was sent to it.
    const message = err.message
    if (err.status === 429) {
      setRetrySeconds(err.retryAfterSeconds ?? 0)
      setModalError(message)
      setAnnouncement('Too many attempts. The button comes back shortly.')
      return
    }
    switch (err.code()) {
      case 'auth_failed':
      case 'key_required':
        if (where === 'filter') setModalError(message)
        else setKeyError(message)
        setStep((s) => (needsKey && s !== 'entry' ? 'entry' : s))
        break
      // The vendor answered; this is its verdict on the key, not a network
      // fault. Placed like auth_failed, beside the key field, key preserved.
      case 'vendor_error': {
        const v = err.vendorError()
        const copy = vendorErrorCopy(vendorName, message, v?.status ?? null, v?.type ?? null)
        if (where === 'filter') setModalError(copy)
        else setKeyError(copy)
        setStep((s) => (needsKey && s !== 'entry' ? 'entry' : s))
        setAnnouncement(copy)
        return
      }
      // An organisation-level Anthropic key with no workspace named. The
      // answer is the workspace field, so that is where it is said and where
      // focus goes; the key field is left exactly as it was.
      case 'workspace_required': {
        const copy = workspaceRequiredCopy(message)
        if (takesWorkspace) {
          setWorkspaceError(copy)
          setFocusWorkspace(true)
        } else {
          setModalError(copy)
        }
        setStep('entry')
        setAnnouncement(copy)
        return
      }
      case 'unreachable':
        setModalError(message)
        break
      case 'filter_matched_nothing':
      case 'filter_too_broad':
        setFilterError(message)
        break
      case 'gateway_too_old':
        setOldGateway(true)
        break
      // The api will not take a key over plain HTTP. Its sentence goes in the
      // modal's error region; the field is emptied by the caller, and the key
      // form is replaced by the command rather than left there disabled.
      case 'insecure_transport':
        setRefusedTransport(true)
        setModalError(message)
        clearKey()
        break
      case 'partially_installed':
        setPartial(message)
        setStep('result')
        break
      case 'models_enabled':
        setBlockingModels(err.enabledModels())
        break
      default:
        setModalError(message)
    }
    setAnnouncement(message)
  }

  // --- what the checklist shows ---------------------------------------------

  /** The rows that can be catalogued, grouped by family in the order the
   *  vendor listed them. Rows the gateway would never offer — embeddings,
   *  transcription — are not on the list: ticking one would do nothing.
   *  A catalogue of one-model "families" is swept into one heading rather than
   *  given a heading per row — see groupAdmittedModels. */
  const groups = useMemo(
    () => groupAdmittedModels(verification?.admitted ?? []),
    [verification],
  )

  const admitted = verification?.admitted ?? []
  const chosen = admitted.filter((id) => ticked.has(id))

  /** What will actually be sent: family globs where one describes a family
   *  exactly, ids everywhere else. Shown, because an administrator who ticks
   *  a whole family should be able to see that the filter says so — and
   *  because a filter nobody can read is a filter nobody can check. */
  const patterns = useMemo(
    () => serialiseModelFilter(verification?.reachable ?? [], ticked),
    [verification, ticked],
  )

  const toggle = (id: string) =>
    setTicked((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const toggleFamily = (ids: string[], on: boolean) =>
    setTicked((prev) => {
      const next = new Set(prev)
      for (const id of ids) {
        if (on) next.add(id)
        else next.delete(id)
      }
      return next
    })

  // --- the four submissions ---------------------------------------------------

  /** The workspace field's verdict before anything is sent. True when it is
   *  fine (or not asked for); otherwise the error is placed and focused. */
  const workspaceOk = (): boolean => {
    if (!takesWorkspace) return true
    const problem = workspaceIdProblem(workspaceId)
    if (!problem) return true
    setWorkspaceError(problem)
    setAnnouncement(problem)
    workspaceRef.current?.focus()
    return false
  }
  const workspaceToSend = () =>
    takesWorkspace ? workspaceIdToSend(vendor, workspaceId, currentWorkspaceId) : undefined

  const check = async () => {
    const raw = keyRef.current?.value ?? ''
    const problem = vendorKeyProblem(raw)
    if (problem) {
      setKeyError(problem)
      setAnnouncement(problem)
      keyRef.current?.focus()
      return
    }
    if (!workspaceOk()) return
    setKeyError(null)
    setWorkspaceError(null)
    setModalError(null)
    setClearedAfterFailure(false)
    setStep('checking')
    try {
      const result = await verifyVendorKey(vendor, {
        key: raw.trim(),
        workspace_id: workspaceToSend(),
      })
      setVerification(result)
      // Everything is ticked on a first check of an INSTALL: someone who opened
      // "Install" wants this vendor, and the list is here to be trimmed rather
      // than filled in. A Replace has a selection already and opens on it —
      // seeding from `prev` alone ticked everything the new key could reach,
      // which quietly widened the filter on the way past. A second check within
      // the session keeps what was chosen instead of undoing it.
      setTicked((prev) =>
        seedTickedModels({
          admitted: result.admitted,
          reachable: result.reachable,
          currentFilter,
          currentModelIds,
          previous: prev,
        }),
      )
      setStep('checked')
      setAnnouncement(
        result.admitted.length === 1
          ? `Key accepted. 1 model is available from ${vendorName}.`
          : `Key accepted. ${result.admitted.length} models are available from ${vendorName}.`,
      )
    } catch (err) {
      applyError(err, 'key')
      setStep('entry')
    }
  }

  const install = async () => {
    const raw = keyRef.current?.value ?? ''
    if (raw.trim() === '') {
      // The field was cleared behind us — a browser restoring the page, or a
      // password manager. Sending an empty key would store nothing and say
      // nothing useful about why.
      setStep('entry')
      setKeyError('The key field is empty. Paste the key again.')
      return
    }
    if (!workspaceOk()) {
      setStep('entry')
      return
    }
    setStep('saving')
    setModalError(null)
    setFilterError(null)
    setAnnouncement('Storing the key.')
    try {
      const result = await installVendorKey(vendor, {
        key: raw.trim(),
        model_filter: patterns,
        workspace_id: workspaceToSend(),
      })
      onDone({
        kind: 'installed',
        admitted: result.admitted,
        installingUntil: result.installing_until,
        warnings: result.warnings ?? [],
      })
      close()
    } catch (err) {
      applyError(err, 'key')
      // The key is cleared below whatever happened, so every retry starts at
      // the field. Anything else leaves a disabled, empty field above a button
      // that would report the key as missing.
      setClearedAfterFailure(true)
      setStep((s) => (s === 'result' ? 'result' : 'entry'))
    } finally {
      // The key does not stay in the field. A failed install is retried by
      // pasting again, which costs a moment; a secret left sitting in a form
      // behind an error message costs more than that.
      clearKey()
    }
  }

  const saveFilter = async () => {
    if (!workspaceOk()) return
    setStep('saving')
    setFilterError(null)
    setModalError(null)
    setAnnouncement('Saving the model selection.')
    try {
      const before = admitted.filter((id) => currentModelIds.includes(id))
      const result = await setVendorFilter(vendor, patterns, workspaceToSend())
      setDiff(admittedDiff(before, result.admitted))
      setStep('result')
      onDone({ kind: 'filter', admitted: result.admitted })
      setAnnouncement('Model selection saved.')
    } catch (err) {
      applyError(err, 'filter')
      // A missing workspace sends the field back to being editable, and the
      // checklist with it: the models on offer may differ under another one.
      setStep(err instanceof ApiError && err.code() === 'workspace_required' ? 'entry' : 'checked')
    }
  }

  /** Change models' check again, for after a workspace was entered. */
  const recheckInstalled = () => {
    if (!workspaceOk()) return
    void verifyInstalled(workspaceId)
  }

  const remove = async (disableModels: boolean) => {
    setStep('saving')
    setModalError(null)
    setAnnouncement(`Removing the ${vendorName} key.`)
    try {
      const { warnings } = await removeVendorKey(vendor, { disableModels })
      onDone({ kind: 'removed', warnings })
      close()
    } catch (err) {
      applyError(err, 'remove')
      setStep('entry')
    }
  }

  // --- copy -------------------------------------------------------------------

  const title = (() => {
    if (partial) return 'The key was stored; the model selection was not'
    if (diff) return 'Models updated'
    switch (mode) {
      case 'install':
        return `Install an ${vendorName} key`
      case 'replace':
        return `Replace the ${vendorName} key`
      case 'change-models':
        return `Choose which ${vendorName} models to offer`
      case 'remove':
        return `Remove the ${vendorName} key`
    }
  })()

  const description = (() => {
    if (partial || diff) return undefined
    switch (mode) {
      case 'install':
        return `Darkplum stores the key in your cluster and calls ${vendorName} with it. Nothing is sent anywhere until you enable models afterwards.`
      case 'replace':
        return `The new key is checked against ${vendorName} before anything is stored, so a key that does not work cannot replace one that does.`
      case 'change-models':
        return `Only the models you tick are catalogued. The key is not involved and is not asked for.`
      case 'remove':
        return `Darkplum keeps your model selection, so reinstalling a key does not mean choosing the models again.`
    }
  })()

  const busy = step === 'checking' || step === 'saving'
  /** This modal cannot ask for a key: the api would refuse it over the
   *  transport this page is on. Only the two modes that carry one are
   *  affected — Change models and Remove send no key and still work. */
  const keyBlockedByTransport = needsKey && (!transportOk || refusedTransport)
  /**
   * Which sentence explains it.
   *
   * A null help with a refusal in hand is the 403 case: the api advertised a
   * transport it would carry a key over and then refused one anyway. Only the
   * api can be wrong about its own setting, so that reads as the api-side
   * fault — which is also the honest reading when the flag was simply absent
   * and this page is HTTPS.
   */
  const transportHelpKind: KeyTransportHelp = transportHelp ?? 'api_not_https'
  // Derived from the fingerprint rather than from the ref: the ref does not
  // re-render, and a stale read here would arm Escape over a field that has
  // something in it.
  const keyTyped = fingerprint !== null
  // Disarmed while there is a key in the field or a request is in flight:
  // both are states where a stray Escape costs something.
  const dismissible = !busy && !keyTyped

  const confirmMatches = confirmText.trim().toLowerCase() === vendor

  /**
   * The models that stand between this key and being removed.
   *
   * The catalogue already knows which of this vendor's rows are on, so the
   * removal says so before it is attempted rather than after a refusal — an
   * administrator should not have to press a destructive button to discover
   * what it is going to do. The api's own 409 list still wins when it arrives:
   * it is the authority, and it may know about a row this page has not synced.
   */
  const blocking =
    blockingModels ?? (mode === 'remove' && enabledModelIds.length > 0 ? enabledModelIds : null)

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
        {/* One region, one voice. Updated on transitions only — see above. */}
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

        {/* An api talking to a gateway that predates this. Today's path, and
            one sentence about why it is the one on screen — not a form that
            would fail on submit. */}
        {oldGateway && (
          <div className={styles.notice}>
            <p className={styles.noticeText}>
              The gateway running in this cluster is older than this screen and cannot take a key
              from it. Install the key with this command; everything else on this page works as it
              does now.
            </p>
            <CodeBlock>
              <code>{VENDOR_KEY_COMMAND[vendor]}</code>
            </CodeBlock>
          </div>
        )}

        {/* The key was stored and the selection was not. Said as what is true
            now and what to do, because "partially_installed" is a state
            somebody has to finish, not a failure to retry blindly. */}
        {partial && (
          <div className={styles.failure}>
            <p className={styles.failureTitle}>{partial}</p>
            <p className={styles.text}>
              The key is stored and verified. {vendorName} is offering whatever the previous
              selection admitted. Open <strong>Change models</strong> on the card to set it — the
              key is not needed again.
            </p>
          </div>
        )}

        {/* The transport will not carry a key. Known before anything is typed
            (the models response says so) or learned from a 403 mid-flow; both
            read the same on screen, because both mean the same thing and have
            the same answer. */}
        {keyBlockedByTransport && (
          <div className={styles.notice}>
            <p className={styles.noticeText}>{KEY_TRANSPORT_HELP_TEXT[transportHelpKind]}</p>
            {/* The port-forward first: it is the one remedy that works in both
                cases, needs no certificate, and leaves this same screen able
                to take the key a minute from now. */}
            <CodeBlock>
              <code>{WEB_PORT_FORWARD_COMMAND}</code>
            </CodeBlock>
            <CodeBlock>
              <code>{VENDOR_KEY_COMMAND[vendor]}</code>
            </CodeBlock>
          </div>
        )}

        {modalError && !oldGateway && (
          <p className={styles.error} role="alert">{modalError}</p>
        )}

        {/* --- the key field ---------------------------------------------
            Rendered on every step of an install, never only on the first.
            Unmounting it at the checklist step would take the value with it —
            the field is uncontrolled, so the DOM node IS the storage — and the
            install would submit an empty key. Disabled past the check instead,
            which keeps the value, keeps the fingerprint line on screen while
            the decision is made, and says plainly that this part is settled. */}
        {needsKey && !partial && !oldGateway && !keyBlockedByTransport && (
          <>
            <Input
              ref={keyRef}
              type="password"
              label={mode === 'replace' ? `New ${vendorName} API key` : `${vendorName} API key`}
              // Nothing about this field is a credential a manager should
              // offer to fill or offer to save: it is an organisation's key
              // being typed once into an admin screen.
              autoComplete="new-password"
              name="vendor-key-entry"
              spellCheck={false}
              autoCorrect="off"
              autoCapitalize="off"
              data-1p-ignore=""
              data-lpignore="true"
              disabled={busy || step === 'checked'}
              error={keyError ?? undefined}
              aria-describedby={`${vendor}-key-shape`}
              onChange={(e) => {
                setFingerprint(keyFingerprint(e.currentTarget.value))
                setClearedAfterFailure(false)
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  void check()
                }
              }}
            />
            {/* The only thing said about a key nobody can see, in two forms:
                the visible one leads with dots so the tail reads as a tail,
                the spoken one drops them, because a screen reader announcing
                three middle dots one at a time is noise.

                Each audience gets exactly one of them, and the field points at
                the spoken node alone. Both used to sit inside the described
                element, which left the description depending on an AT honouring
                aria-hidden while it flattened the subtree — read one way that
                is the line twice over. The visible paragraph is hidden from the
                tree outright; the spoken span is the only thing named. */}
            <p className={styles.shape} aria-hidden="true">
              {fingerprint?.text ?? 'Nothing pasted yet'}
            </p>
            <span className={styles.srOnly} id={`${vendor}-key-shape`}>
              {fingerprint?.spoken ?? 'Nothing pasted yet'}
            </span>
            {clearedAfterFailure && (
              <p className={styles.hint}>
                The field was emptied when the attempt failed. Paste the key again to retry —
                nothing else you chose has been lost.
              </p>
            )}
            {(step === 'entry' || step === 'checking') && (
              <p className={styles.hint}>
                The key is checked against {vendorName} through the gateway — the same network path
                it will use in production — before anything is stored. It is never shown again,
                here or anywhere else.
              </p>
            )}
          </>
        )}

        {/* --- the workspace field ----------------------------------------
            Anthropic only, and optional: an organisation-level key is refused
            unless every request names a workspace. Disabled past the check
            for the same reason the key field is — what the vendor said was
            said about this key in this workspace. */}
        {takesWorkspace &&
          !partial &&
          !diff &&
          !oldGateway &&
          !keyBlockedByTransport && (
            <>
              <Input
                ref={workspaceRef}
                label="Workspace ID (optional)"
                name="vendor-workspace-id"
                value={workspaceId}
                autoComplete="off"
                autoCorrect="off"
                autoCapitalize="off"
                spellCheck={false}
                maxLength={64}
                disabled={busy || step === 'checked'}
                error={workspaceError ?? undefined}
                aria-invalid={workspaceError ? true : undefined}
                aria-describedby={`${vendor}-workspace-help`}
                onChange={(e) => setWorkspaceId(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key !== 'Enter') return
                  e.preventDefault()
                  if (needsKey) void check()
                  else if (step === 'entry') recheckInstalled()
                }}
              />
              <p className={styles.hint} id={`${vendor}-workspace-help`}>
                Only needed when the key was created at organisation level rather than inside a
                workspace. Find it under Workspaces in {vendorName}'s console (
                <code className={styles.itemId}>wrkspc_…</code>).
              </p>
            </>
          )}

        {/* --- the checklist --------------------------------------------- */}
        {step === 'checking' && (
          <p className={styles.text}>Checking with {vendorName}…</p>
        )}

        {(step === 'checked' || step === 'saving') && !partial && (
          <>
            {admitted.length === 0 ? (
              <p className={styles.text}>
                {vendorName} answered, but published no model this gateway can offer. Nothing to
                choose from, so there is nothing to store.
              </p>
            ) : (
              <fieldset className={styles.picker}>
                <legend className={styles.legend}>
                  Models to offer — {chosen.length} of {admitted.length} ticked
                </legend>
                <div className={styles.list}>
                  {groups.map((group) => {
                    const all = group.ids.every((id) => ticked.has(id))
                    // The swept group is named after the vendor, not after a
                    // family: its members have nothing in common but being the
                    // only one of their kind.
                    const heading = group.family
                      ? formatModelName(group.family)
                      : `Other ${vendorName} models`
                    return (
                      <div key={group.family ?? '\u0000other'} className={styles.group}>
                        <div className={styles.groupHead}>
                          <span className={styles.groupName}>{heading}</span>
                          {group.ids.length > 1 && (
                            <button
                              type="button"
                              className={styles.groupToggle}
                              disabled={busy}
                              onClick={() => toggleFamily(group.ids, !all)}
                            >
                              {all ? 'None' : 'All'}
                              <span className={styles.srOnly}> in {heading}</span>
                            </button>
                          )}
                        </div>
                        <ul className={styles.items}>
                          {group.ids.map((id) => (
                            <li key={id}>
                              <label className={styles.item}>
                                <input
                                  type="checkbox"
                                  className={styles.checkbox}
                                  checked={ticked.has(id)}
                                  disabled={busy}
                                  onChange={() => toggle(id)}
                                />
                                <span className={styles.itemText}>
                                  <span className={styles.itemName}>{formatModelName(id)}</span>
                                  <code className={styles.itemId}>{id}</code>
                                </span>
                              </label>
                            </li>
                          ))}
                        </ul>
                      </div>
                    )
                  })}
                </div>
              </fieldset>
            )}

            {chosen.length > 0 && (
              <p className={styles.stored}>
                Stored as: <code className={styles.storedValue}>{patterns.join(', ')}</code>
              </p>
            )}

            {filterError && <p className={styles.error} role="alert">{filterError}</p>}

            {/* The spend boundary, on the step where the button that crosses
                it is. Named vendor, named console: Darkplum has no limit of
                its own to offer and must not imply otherwise. */}
            {needsKey && (
              <div className={styles.boundary}>
                <p className={styles.boundaryText}>
                  Darkplum cannot see how this key is scoped at {vendorName}, and it sets no spend
                  limit of its own. Use a project-scoped key with a limit set in {vendorName}'s own
                  console.
                </p>
                <p className={styles.boundaryChange}>
                  Storing the key does not send anything to {vendorName}. Models are offered only
                  after you enable them, which is one step from the card behind this.
                </p>
              </div>
            )}
          </>
        )}

        {/* --- remove ------------------------------------------------------ */}
        {mode === 'remove' && !partial && (
          <>
            <p className={styles.text}>
              {vendorName} models stop answering as soon as the gateway picks this up, usually about
              two minutes. Requests to them are refused by name — nothing falls back to another
              vendor.
            </p>

            {/* The half of the removal Darkplum cannot perform. Forgetting a
                key is not revoking it: it stays live at the vendor, billable
                by anyone else holding a copy, and the reason somebody is
                removing it is often exactly that somebody else holds one. */}
            <p className={styles.text}>
              Removing it here does not invalidate it — revoke the key in {vendorName}'s console
              afterwards.
            </p>

            {blocking && blocking.length > 0 && (
              <div className={styles.failure}>
                <p className={styles.failureTitle}>
                  {blocking.length === 1
                    ? '1 model from this vendor is still enabled. Removing the key turns it off:'
                    : `${blocking.length} models from this vendor are still enabled. Removing the key turns them off:`}
                </p>
                <ul className={styles.idList}>
                  {blocking.map((id) => (
                    <li key={id}><code className={styles.itemId}>{id}</code></li>
                  ))}
                </ul>
              </div>
            )}

            {/* The name goes in the sentence, not in the label: labels on
                this form are uppercased by the design system, and a typed
                confirmation that shows you ANTHROPIC while expecting
                anthropic is a trap. Matched case-insensitively anyway. */}
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

        {/* --- what a filter change did ------------------------------------ */}
        {diff && (
          <div role="status">
            {diff.unchanged ? (
              <p className={styles.text}>
                The same {diff.kept} {diff.kept === 1 ? 'model is' : 'models are'} on offer. Nothing
                changed.
              </p>
            ) : (
              <>
                {diff.added.length > 0 && (
                  <>
                    <p className={styles.resultTitle}>
                      {diff.added.length === 1 ? 'Now offered:' : `Now offered (${diff.added.length}):`}
                    </p>
                    <ul className={styles.idList}>
                      {diff.added.map((id) => (
                        <li key={id}><code className={styles.itemId}>{id}</code></li>
                      ))}
                    </ul>
                  </>
                )}
                {diff.removed.length > 0 && (
                  <>
                    <p className={styles.resultTitle}>
                      {diff.removed.length === 1
                        ? 'No longer offered:'
                        : `No longer offered (${diff.removed.length}):`}
                    </p>
                    <ul className={styles.idList}>
                      {diff.removed.map((id) => (
                        <li key={id}><code className={styles.itemId}>{id}</code></li>
                      ))}
                    </ul>
                    <p className={styles.hint}>
                      A model that is no longer offered leaves the catalogue at the next sync. Any
                      request naming it is refused by name.
                    </p>
                  </>
                )}
              </>
            )}
          </div>
        )}

        {/* --- footer ------------------------------------------------------- */}
        <div className={styles.footer}>
          {step === 'result' || oldGateway || keyBlockedByTransport ? (
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

              {/* Editing the key invalidates what the vendor said about it,
                  so going back drops the verification with it rather than
                  installing a new key against an old model list. */}
              {needsKey && step === 'checked' && (
                <Button
                  type="button"
                  variant="ghost"
                  onClick={() => {
                    setVerification(null)
                    setTicked(new Set())
                    setFilterError(null)
                    setStep('entry')
                    setAnnouncement(
                      takesWorkspace
                        ? 'Editing the key and workspace. Check again before installing.'
                        : 'Editing the key. Check it again before installing.',
                    )
                  }}
                >
                  {takesWorkspace ? 'Change key or workspace' : 'Use a different key'}
                </Button>
              )}

              {/* Change models has no key to re-enter, but a workspace can be
                  wrong or missing: these two are the way back to the field and
                  the check after it. */}
              {mode === 'change-models' && takesWorkspace && step === 'checked' && (
                <Button
                  type="button"
                  variant="ghost"
                  onClick={() => {
                    setVerification(null)
                    setFilterError(null)
                    setStep('entry')
                    setAnnouncement('Editing the workspace. Check again before saving.')
                  }}
                >
                  Change workspace
                </Button>
              )}

              {mode === 'change-models' && (step === 'entry' || step === 'checking') && (
                <Button
                  type="button"
                  onClick={recheckInstalled}
                  disabled={busy || retrySeconds > 0}
                >
                  {step === 'checking'
                    ? 'Checking…'
                    : retrySeconds > 0
                      ? `Try again in ${formatRetryCountdown(retrySeconds)}`
                      : 'Check again'}
                </Button>
              )}

              {needsKey && (step === 'checked' || step === 'saving') && (
                <Button
                  type="button"
                  variant="danger"
                  onClick={() => void install()}
                  disabled={busy || chosen.length === 0 || retrySeconds > 0}
                >
                  {step === 'saving'
                    ? 'Storing…'
                    : retrySeconds > 0
                      ? `Try again in ${formatRetryCountdown(retrySeconds)}`
                      : mode === 'replace'
                        ? 'Replace key'
                        : 'Install key'}
                </Button>
              )}

              {mode === 'change-models' && (step === 'checked' || step === 'saving') && (
                <Button
                  type="button"
                  onClick={() => void saveFilter()}
                  disabled={busy || chosen.length === 0}
                >
                  {step === 'saving' ? 'Saving…' : 'Save selection'}
                </Button>
              )}

              {mode === 'remove' && (
                <Button
                  type="button"
                  variant="danger"
                  onClick={() => void remove((blocking?.length ?? 0) > 0)}
                  disabled={busy || !confirmMatches}
                >
                  {step === 'saving'
                    ? 'Removing…'
                    : blocking && blocking.length > 0
                      ? `Turn ${blocking.length === 1 ? 'this model' : `these ${blocking.length} models`} off and remove the key`
                      : 'Remove key'}
                </Button>
              )}
            </>
          )}
        </div>
      </div>
    </Modal>
  )
}
