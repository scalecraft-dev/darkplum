import { useEffect, useRef, useState } from 'react'
import type { ProviderState, ProviderStatus, VendorName } from '../types'
import { VENDORS, VENDOR_KEY_COMMAND, vendorLabel } from '../lib/modelRouting'
import {
  KEY_TRANSPORT_HELP_TEXT,
  WEB_PORT_FORWARD_COMMAND,
  installPhase,
  installProgress,
  removingDetail,
  supportsUiKeyInstall,
} from '../lib/vendorKeys'
import type { KeyTransportHelp } from '../lib/vendorKeys'
import { Badge, Button, CodeBlock } from '../components/ui'
import type { VendorInstallMode } from './VendorInstallModal'
import styles from './VendorStatusSection.module.css'

/**
 * Model vendors — whether Darkplum can currently reach Anthropic and OpenAI
 * with the organisation's keys, and the place those keys are set.
 *
 * Both vendors are always listed, configured or not. A row that appears only
 * once something exists cannot be used to find out that nothing does, and
 * "where would I even set this up?" is the first question this section exists
 * to answer.
 *
 * A vendor that is not working never stops the gateway. That is the whole
 * design, and it means the only place an operator learns their key is wrong is
 * here — so every failed state says what happened and what to do about it, in
 * words, not in a status code.
 *
 * This card also owns the wait. An install is stored immediately and reaches
 * the gateway a minute or two later, so the modal closes and the progress
 * lives here: it is derived from the api's `installing_until` rather than from
 * a timer started in the browser, which is what makes it survive a reload, a
 * second tab, and somebody else in the same organisation watching it.
 */

interface Props {
  providers: ProviderStatus[]
  /** What the gateway behind this api can do. Without "ui_key_install" every
   *  install route answers 409, so this card offers the command line instead
   *  of a form that would fail on submit. */
  providerFeatures: string[]
  /** False when the api would refuse a key over this page's transport. The
   *  card then shows the way round it for the two states that would otherwise
   *  offer a key form — a form that the api answers 403 to is worse than no
   *  form at all. Nothing else on the card changes: a filter change and a
   *  removal carry no key, and both keep working. */
  transportOk: boolean
  /** Which of the two transport faults this is, for the sentence above that
   *  command. Null when a key can be installed from here. */
  transportHelp: KeyTransportHelp | null
  loading: boolean
  /** The api's own warnings from the last removal, by vendor. Shown verbatim
   *  while that removal is reaching the gateway, because one of them decides
   *  whether this vendor's models are already off or still answering. */
  removalWarnings: Partial<Record<VendorName, string[]>>
  /** Vendors with at least one catalogued model that is not enabled yet —
   *  the only case where "Review and enable" has anything to do. */
  enableableVendors: Set<VendorName>
  onManage: (vendor: VendorName, mode: VendorInstallMode) => void
  onReviewAndEnable: (vendor: VendorName) => void
  /** Re-reads the vendor block now. */
  onCheckAgain: () => void
  /** Stores the filter the command line set, so the UI becomes the writer of
   *  record for it. Rejects with the api's own message. */
  onAdoptFilter: (vendor: VendorName, filter: string[]) => Promise<void>
}

interface StateCopy {
  label: string
  variant: 'success' | 'warning' | 'danger' | 'neutral' | 'info'
  dashed?: boolean
  /** What is true right now. */
  detail: string
  /** What to do next, or null when there is nothing to do. */
  next: string | null
}

/**
 * What is true, and what to do about it.
 *
 * Two capabilities, not one. Installing a key needs both the gateway feature
 * and a transport the api will carry a key over; changing which models are
 * offered needs neither — it sends no key at all. Collapsing them into one
 * flag put "re-run the install command with MODELS set" directly above a
 * working Change models button, which is an instruction to do the hard thing
 * for no reason.
 */
function stateCopy(p: ProviderStatus, canInstallKey: boolean, canChangeFilter: boolean): StateCopy {
  const vendor = vendorLabel(p.name)
  switch (p.state) {
    case 'ok':
      return {
        label: 'Connected',
        variant: 'success',
        detail:
          p.model_count === 1
            ? `1 model from ${vendor} is in the catalogue below.`
            : `${p.model_count} models from ${vendor} are in the catalogue below.`,
        next: null,
      }
    case 'not_configured':
      return {
        label: 'No key installed',
        variant: 'neutral',
        dashed: true,
        // Not "none of their models appear in the catalogue": right after a
        // removal they are all still listed below, and an administrator who
        // reads this and then scrolls has been told something they can see is
        // untrue. What is actually gone is the ability to answer.
        detail: `Darkplum holds no ${vendor} key, so none of their models can answer. Rows already in the catalogue below stay until the next sync and are refused by name.`,
        next: canInstallKey
          ? `Install one here. The key is checked against ${vendor} through the gateway before it is stored, and it is never shown again.`
          : `Install one with the command below. It reads the key from your environment or from standard input — never from the command line — and checks it against ${vendor} before storing it.`,
      }
    case 'filter_missing':
      return {
        label: 'No models chosen',
        variant: 'warning',
        detail: `The key works, but nothing says which ${vendor} models to offer, so none are offered.`,
        next: canChangeFilter
          ? 'Choose them with Change models. Darkplum does not catalogue a vendor\'s whole model list by default — an explicit choice is what keeps the catalogue, and the bill, bounded.'
          : `Re-run the install command with MODELS set. Darkplum does not catalogue a vendor's whole model list by default — an explicit list is what keeps the catalogue, and the bill, bounded.`,
      }
    case 'filter_matched_nothing':
      return {
        label: 'Nothing matched',
        variant: 'warning',
        detail: `The key works and a selection is stored, but no ${vendor} model matches it any more.`,
        next: canChangeFilter
          ? 'Open Change models and tick from the list the vendor publishes today — a model that has been retired matches nothing.'
          : `Re-run the install command with globs that match the model names ${vendor} currently publishes — a name that has been retired matches nothing.`,
      }
    case 'auth_failed':
      return {
        label: 'Key rejected',
        variant: 'danger',
        detail: `${vendor} refused the key Darkplum holds. It may have been revoked, rotated, or copied from the wrong account.`,
        next: canInstallKey
          ? 'Replace it with a working one. Requests to this vendor are refused until one is accepted.'
          : 'Install the key again with the command below. Requests to this vendor are refused until it is accepted.',
      }
    case 'unreachable':
      return {
        label: 'Not reachable',
        variant: 'warning',
        detail: `Darkplum could not reach ${vendor}'s API on its last attempt.`,
        next: "This usually clears on its own. If it does not, check the gateway's outbound network path and any proxy it is meant to use.",
      }
    default:
      // A state this bundle has not met. Named as unknown rather than dropped:
      // a vendor row that vanishes because the server grew a word reads as a
      // vendor that was never configured, which is a different problem.
      return {
        label: 'Unrecognised state',
        variant: 'neutral',
        dashed: true,
        detail: `The gateway reported a state this page does not know: ${String(p.state as ProviderState)}.`,
        next: 'Update the web bundle, or read the gateway log for the detail.',
      }
  }
}

function lastSync(at: string | null): string {
  if (!at) return 'Never synced'
  const when = new Date(at)
  if (Number.isNaN(when.getTime())) return 'Never synced'
  return `Checked ${when.toLocaleString()}`
}

export function VendorStatusSection({
  providers,
  providerFeatures,
  transportOk,
  transportHelp,
  loading,
  removalWarnings,
  enableableVendors,
  onManage,
  onReviewAndEnable,
  onCheckAgain,
  onAdoptFilter,
}: Props) {
  const canInstall = supportsUiKeyInstall(providerFeatures)

  // Both vendors, in a fixed order, whatever the response contained.
  const rows = VENDORS.map(
    (name) =>
      providers.find((p) => p.name === name) ?? {
        name,
        configured: false,
        state: 'not_configured' as const,
        model_count: 0,
        last_sync_at: null,
        filter_source: 'none' as const,
        filter: [],
        installing_until: null,
        removing_until: null,
      },
  )

  /**
   * A clock, ticking only while something is actually in flight.
   *
   * The progress below is a function of `installing_until` and the current
   * time, so it needs the second hand to move; it does not need it to move
   * when nothing is waiting on it, and a permanent interval on an admin page
   * that is often left open is a permanent render loop. A removal needs it for
   * the other end of the same window: when the stamp runs out the card has to
   * settle by itself rather than wait for the next poll.
   */
  const [now, setNow] = useState(() => Date.now())
  const anyInFlight = rows.some((p) => {
    const phase = installPhase(p, now)
    return phase === 'installing' || phase === 'removing'
  })
  useEffect(() => {
    if (!anyInFlight) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [anyInFlight])

  return (
    <section className={styles.section} aria-labelledby="model-vendors-heading">
      <div className={styles.head}>
        <h2 id="model-vendors-heading" className={styles.title}>Model vendors</h2>
        <p className={styles.subtitle}>
          Anthropic and OpenAI, called at their own APIs with a key your organisation installs.
          Prompts sent to them leave your cloud account. A vendor that is not working is reported
          here and never takes the gateway down with it.
        </p>
        {/* The boundary of this card, in one sentence rather than a banner:
            somebody who came here to add a model needs to know this is not
            where self-hosted ones are added, and nothing more than that. */}
        <p className={styles.scope}>
          Vendor keys and vendor models only. Self-hosted models and AWS Bedrock model patterns are
          set when the platform is deployed, not from this page.
        </p>
      </div>

      <div className={styles.rows}>
        {rows.map((p) => (
          <VendorRow
            key={p.name}
            provider={p}
            now={now}
            loading={loading}
            canInstall={canInstall}
            transportOk={transportOk}
            transportHelp={transportHelp}
            removalWarnings={removalWarnings[p.name] ?? EMPTY_WARNINGS}
            enableable={enableableVendors.has(p.name)}
            onManage={onManage}
            onReviewAndEnable={onReviewAndEnable}
            onCheckAgain={onCheckAgain}
            onAdoptFilter={onAdoptFilter}
          />
        ))}
      </div>
    </section>
  )
}

/** One array, so a vendor with no warnings gets the same reference every
 *  render and the row's announcement effect does not re-run on each poll. */
const EMPTY_WARNINGS: string[] = []

interface RowProps {
  provider: ProviderStatus
  now: number
  loading: boolean
  canInstall: boolean
  transportOk: boolean
  transportHelp: KeyTransportHelp | null
  removalWarnings: string[]
  enableable: boolean
  onManage: (vendor: VendorName, mode: VendorInstallMode) => void
  onReviewAndEnable: (vendor: VendorName) => void
  onCheckAgain: () => void
  onAdoptFilter: (vendor: VendorName, filter: string[]) => Promise<void>
}

function VendorRow({
  provider: p,
  now,
  loading,
  canInstall,
  transportOk,
  transportHelp,
  removalWarnings,
  enableable,
  onManage,
  onReviewAndEnable,
  onCheckAgain,
  onAdoptFilter,
}: RowProps) {
  const vendor = vendorLabel(p.name)
  const phase = loading ? 'settled' : installPhase(p, now)
  // What "next" says has to match which controls are actually on the card, so
  // each sentence is decided by the capability its own button needs.
  //
  // Installing or replacing a key needs the gateway feature AND a transport
  // the api will carry a key over; an old gateway and a plain-HTTP page both
  // end at the command line, and "Install one here" over a card with no
  // Install button is the sentence to avoid. Choosing which models are offered
  // needs only the feature: no key crosses the wire, so the transport has no
  // say in it and Change models is right there.
  const canInstallKey = canInstall && transportOk
  const canChangeFilter = canInstall
  const copy = loading
    ? { label: 'Checking…', variant: 'neutral' as const, dashed: true, detail: '', next: null }
    : stateCopy(p, canInstallKey, canChangeFilter)

  const [adopting, setAdopting] = useState(false)
  const [adoptError, setAdoptError] = useState<string | null>(null)

  /**
   * One polite region for this card, spoken when the card changes state and
   * at no other time.
   *
   * The install is the reason it exists: the modal closes while the work is
   * still happening, so for anyone not watching the bar, this is the only
   * announcement that the key landed — or that it has not.
   */
  const [announcement, setAnnouncement] = useState('')
  const lastAnnounced = useRef<string>('')
  useEffect(() => {
    if (loading) return
    const key = `${phase}:${phase === 'settled' ? p.state : ''}`
    if (key === lastAnnounced.current) return
    // The first pass after a load is the page arriving, not a transition.
    const first = lastAnnounced.current === ''
    lastAnnounced.current = key
    if (first) return
    setAnnouncement(
      phase === 'installing'
        ? `${vendor}: installing the key. This usually takes about two minutes.`
        : phase === 'taking_longer'
          ? `${vendor}: the key is taking longer than expected to reach the gateway.`
          : phase === 'removing'
            // The api's warnings verbatim, in the same region and on the same
            // transition: they are the part that says whether this vendor's
            // models went off with the key or are still answering.
            ? `${vendor}: removing the key. ${removingDetail(removalWarnings)}${
                removalWarnings.length > 0 ? ` ${removalWarnings.join(' ')}` : ''
              }`
            : `${vendor}: ${stateCopy(p, canInstallKey, canChangeFilter).label}.`,
    )
  }, [phase, p, loading, vendor, canInstallKey, canChangeFilter, removalWarnings])

  const adopt = async () => {
    setAdopting(true)
    setAdoptError(null)
    try {
      await onAdoptFilter(p.name, p.filter)
    } catch (err) {
      setAdoptError(err instanceof Error ? err.message : 'The selection could not be saved.')
    } finally {
      setAdopting(false)
    }
  }

  // The command is offered on the two states it answers, whenever a key cannot
  // be installed from here: an older gateway refuses the route, and plain HTTP
  // makes the api refuse the key. Different reason, same path out, so the
  // sentence above it differs and the block does not.
  const showCommand =
    !loading && !canInstallKey && (p.state === 'not_configured' || p.state === 'auth_failed')

  const badge =
    phase === 'installing'
      ? { label: 'Installing', variant: 'info' as const, dashed: false }
      : phase === 'taking_longer'
        ? { label: 'Taking longer than expected', variant: 'warning' as const, dashed: false }
        : phase === 'removing'
          ? { label: 'Removing', variant: 'warning' as const, dashed: false }
          : { label: copy.label, variant: copy.variant, dashed: copy.dashed }

  const pct = Math.round(installProgress(p.installing_until, now) * 100)

  return (
    <div className={styles.row}>
      <p className={styles.srOnly} role="status">{announcement}</p>

      <div className={styles.rowHead}>
        <span className={styles.vendor}>{vendor}</span>
        <Badge variant={badge.variant} bordered dashed={badge.dashed}>
          {badge.label}
        </Badge>
        {!loading && <span className={styles.sync}>{lastSync(p.last_sync_at)}</span>}
      </div>

      {/* --- installing ------------------------------------------------- */}
      {phase === 'installing' && (
        <>
          <p className={styles.detail}>
            The key is stored. The gateway picks it up on its next sync, usually about two minutes.
            You can leave this page — the progress is kept on the server, not in this tab.
          </p>
          <div
            className={styles.progress}
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={pct}
            aria-label={`Installing the ${vendor} key`}
          >
            <div className={styles.progressFill} style={{ width: `${pct}%` }} />
          </div>
          {/* A key can be valid and still be the wrong one — the right format
              from the wrong account is stored and verified exactly like the
              right one, and the only symptom is models that are not there.
              Rotation is verified against the vendor before anything is
              stored, so there is nothing to protect by making somebody wait
              out a two-minute sync first. Change models and Remove stay hidden:
              one would act on a selection that is still settling and the other
              would start a second errand on top of one in flight. */}
          {canInstallKey && (
            <div className={styles.actions}>
              <Button
                type="button"
                size="sm"
                variant="secondary"
                onClick={() => onManage(p.name, 'replace')}
              >
                Replace key
              </Button>
            </div>
          )}
        </>
      )}

      {/* --- removing ---------------------------------------------------
          The key is already gone from the api; the gateway is a sync behind,
          so the vendor is still reporting itself connected. Nothing is offered
          to press: every action here would either act on a key that no longer
          exists or start a second errand on top of one in flight. */}
      {phase === 'removing' && (
        <>
          <p className={styles.detail}>{removingDetail(removalWarnings)}</p>
          {/* The one thing this page cannot do for them. Darkplum forgetting a
              key is not the vendor forgetting it: it is still live at the
              vendor, still billable, and still usable by anyone who has a
              copy. */}
          <p className={styles.next}>
            Revoke the key in {vendor}'s console too — removing it here does not invalidate it.
          </p>
          {removalWarnings.length > 0 && (
            <ul className={styles.warnings}>
              {removalWarnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          )}
        </>
      )}

      {phase === 'taking_longer' && (
        <>
          <p className={styles.detail}>
            The key is stored, but the gateway has not picked it up yet. This is not an error and
            nothing was lost — a busy or restarting gateway takes longer than usual.
          </p>
          <div className={styles.actions}>
            <Button type="button" size="sm" variant="secondary" onClick={onCheckAgain}>
              Check again
            </Button>
          </div>
        </>
      )}

      {/* --- settled ----------------------------------------------------- */}
      {phase === 'settled' && (
        <>
          {copy.detail && <p className={styles.detail}>{copy.detail}</p>}
          {copy.next && <p className={styles.next}>{copy.next}</p>}

          {/* What is being offered, in the vendor's own words. An operator
              deciding whether a model is missing by accident needs the
              patterns, not a count.
              With no key installed nothing is being offered, and "Offering"
              over a card headed "No key installed" is a flat contradiction.
              The selection is still worth showing — it is kept across a
              removal, which is the reassurance somebody wants immediately
              after one — so it is relabelled rather than dropped. The removing
              phase never reaches here, which is the other place the present
              tense would have been wrong. */}
          {!loading && p.filter.length > 0 && (
            <p className={styles.filter}>
              <span className={styles.filterLabel}>{p.configured ? 'Offering' : 'Will offer'}</span>
              <code className={styles.filterValue}>{p.filter.join(', ')}</code>
            </p>
          )}

          {/* Set from the command line, with a key installed. Not a problem —
              last writer wins in both directions — but it is the reason
              Change models would look as though it had no effect on the next
              deploy, so it is said, with the one-press way to take it over. */}
          {!loading && canChangeFilter && p.configured && p.filter_source === 'env' && (
            <div className={styles.adopt}>
              <p className={styles.adoptText}>
                This selection was set from the command line. Changing it here works, but a later
                run of the install command would overwrite it. Adopting it stores the same
                selection through Darkplum, so this page stays the writer of record.
              </p>
              <div className={styles.actions}>
                <Button
                  type="button"
                  size="sm"
                  variant="secondary"
                  disabled={adopting || p.filter.length === 0}
                  onClick={() => void adopt()}
                >
                  {adopting ? 'Adopting…' : 'Adopt filter'}
                </Button>
              </div>
              {adoptError && <p className={styles.error} role="alert">{adoptError}</p>}
            </div>
          )}

          {showCommand && (
            <>
              <p className={styles.next}>
                {!canInstall
                  ? 'The gateway in this cluster is older than this page, so keys are installed from the command line here. Redeploy the platform to install them from Darkplum.'
                  : KEY_TRANSPORT_HELP_TEXT[transportHelp ?? 'page_not_https']}
              </p>
              {/* The port-forward first, because it is the remedy that takes
                  ten seconds and needs no certificate. Copyable, like every
                  other command on this surface — it is long enough to mistype
                  and the namespace is not guessable. */}
              {canInstall && (
                <div className={styles.command}>
                  <CodeBlock>
                    <code>{WEB_PORT_FORWARD_COMMAND}</code>
                  </CodeBlock>
                </div>
              )}
              <div className={styles.command}>
                <CodeBlock>
                  <code>{VENDOR_KEY_COMMAND[p.name]}</code>
                </CodeBlock>
              </div>
            </>
          )}

          {/* Nothing to draw a row for when the only button in it is the one
              the transport took away. */}
          {!loading && canChangeFilter && (p.configured || canInstallKey) && (
            <div className={styles.actions}>
              {p.state === 'ok' && enableable && (
                <Button type="button" size="sm" onClick={() => onReviewAndEnable(p.name)}>
                  Review and enable
                </Button>
              )}
              {/* Install and Replace are the two that carry a key, so they are
                  the two that go away when the transport cannot carry one.
                  Change models and Remove carry none and stay. The command
                  block above says what to do instead; a disabled button with a
                  tooltip would not. */}
              {!p.configured ? (
                canInstallKey && (
                  <Button type="button" size="sm" onClick={() => onManage(p.name, 'install')}>
                    Install key
                  </Button>
                )
              ) : (
                <>
                  <Button
                    type="button"
                    size="sm"
                    variant={p.state === 'ok' ? 'secondary' : 'primary'}
                    onClick={() => onManage(p.name, 'change-models')}
                    disabled={p.state === 'auth_failed'}
                    title={
                      p.state === 'auth_failed'
                        ? 'The vendor is refusing this key, so it cannot be asked what models it has'
                        : undefined
                    }
                  >
                    Change models
                  </Button>
                  {canInstallKey && (
                    <Button
                      type="button"
                      size="sm"
                      variant={p.state === 'auth_failed' ? 'primary' : 'secondary'}
                      onClick={() => onManage(p.name, 'replace')}
                    >
                      Replace key
                    </Button>
                  )}
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    className={styles.remove}
                    onClick={() => onManage(p.name, 'remove')}
                  >
                    Remove key
                  </Button>
                </>
              )}
            </div>
          )}
        </>
      )}
    </div>
  )
}
