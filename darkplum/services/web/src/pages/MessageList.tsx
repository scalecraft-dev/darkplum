import { memo, useEffect, useId, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import type { BackendClass, ChatMessage, ToolEvent } from '../types'
import { formatModelName } from '../lib/formatModelName'
import {
  RETRY_AFTER_DEFAULT_SECONDS,
  displayClassOf,
  failedVendorLabel,
  formatRetryCountdown,
  isEgress,
  isRouteTargetRefusal,
  routeFallbackOf,
  routerReasonText,
  vendorLabel,
} from '../lib/modelRouting'
import { DATA_RETENTION_CODE, dataRetentionFailureCopy } from '../lib/dataRetention'
import { CodeBlock } from '../components/ui/CodeBlock'
import { CopyButton } from '../components/ui/CopyButton'
import { EgressGlyph } from '../components/ui'
import { KindGlyph } from '../components/ui/KindGlyph'
import styles from './MessageList.module.css'

/** A failed send, kept outside the message history so it is never replayed
 *  back to the model or written to local storage. */
export interface ChatFailure {
  /** The gateway's own error.message — shown verbatim. */
  message: string
  /** OpenAI-style error code, e.g. model_residency_violation. */
  code?: string
  /** The model id the request was sent with. */
  model?: string
  /** Set once a fresh read of the model list came back without that model:
   *  the refusal is not a transient blip, the choice has to be made again. */
  modelGone?: boolean
  /** Who served, or would have served, the turn: the gateway's own provider
   *  header when it got that far, else the picked entry's owner. Only the two
   *  vendors with an organisation key are ever named — see failedVendorLabel. */
  vendor?: string | null
  /** unsupported_parameter only: the request field the gateway refused. */
  param?: string | null
  /** provider_rate_limited only: the vendor's own Retry-After, in seconds.
   *  Null when it sent none and the fixed wait applies instead. */
  retryAfterSeconds?: number | null
  /** model_requires_data_retention only: the concrete model AWS refused —
   *  for a routed alias, not the entry that was picked. */
  refusedModel?: string | null
  /** model_requires_data_retention only: the route that picked it, when the
   *  gateway named one. */
  route?: string | null
}

interface Props {
  /** Item id -> name, for the "used …" line under a reply. */
  libraryNames?: Map<string, string>
  messages: ChatMessage[]
  streaming: boolean
  onRegenerate?: () => void
  failure?: ChatFailure | null
  isAdmin?: boolean
  onRetry?: () => void
  /** Index of the first turn the server did not take, or null while the whole
   *  conversation is stored. */
  unsavedFrom?: number | null
}

/** History is kept on the server; a turn that never got there would otherwise
 *  look identical to one that did. Said once, next to the turn it concerns. */
const UnsavedNotice = () => (
  <p className={styles.unsaved} role="status">
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
    <span>Not saved to your history — send it again to store it.</span>
  </p>
)

/** Headline and next step for each refusal the gateway can return — a routing
 *  decision of the organisation's own, or a failure at the vendor.
 *  When a re-read of the list has confirmed the model is gone, the next step is
 *  the same for everyone — choose another one — so it replaces the per-role
 *  advice, which would otherwise point at a model that no longer exists. */
function failureCopy(
  failure: ChatFailure,
  isAdmin: boolean,
): { title: string; hint: string } {
  // Opens every vendor-failure title, so it is a capitalised phrase rather
  // than a bare name, and is never dropped mid-sentence into a hint.
  const vendor = failedVendorLabel(failure.vendor)
  // A routed alias whose picked route — and the default it would fall back
  // to — cannot answer. The gateway's sentence names both models and is
  // printed verbatim above; this only says what the reader can do.
  if (isRouteTargetRefusal(failure.code, failure.message)) {
    return {
      title: 'This route cannot answer right now',
      hint: isAdmin
        ? 'Pick another model, or retarget the route under Routing.'
        : 'Pick another model, or ask an administrator to retarget the route.',
    }
  }
  switch (failure.code) {
    case 'model_not_found':
      return {
        title: 'That model is not available',
        hint: isAdmin
          ? 'Pick another model, or check that the gateway still discovers this one on the Models page.'
          : 'Pick another model from the list at the top of the page.',
      }
    case 'model_not_enabled':
      return {
        title: 'That model is turned off',
        hint: isAdmin
          ? 'Enable it on the Models page, or pick another model.'
          : 'Pick another model, or ask an administrator to enable this one.',
      }
    case 'model_not_allowed':
      return {
        title: 'That model is not allowed for you',
        hint: isAdmin
          ? "This model is enabled for the organisation, but your own policy — or the policy on the key being used — does not include it. Change it under Users & Keys, or pick a model from the list."
          : 'Your organisation limits which models you can use. Pick one from the list at the top of the page.',
      }
    case 'model_residency_violation':
      // The gateway's own message names the rung in force and the class that
      // conflicts with it — there are three of each now, so nine ways to land
      // here and no single sentence that is true for all of them. It is
      // printed above this hint verbatim; the hint says only what to do, which
      // is the same whichever combination it was.
      return {
        title: 'Blocked by where-it-runs policy',
        hint: isAdmin
          ? 'Pick a model the policy permits, or change where requests may run under Models → Policy. Nothing is sent to a different model instead.'
          : 'Your organisation limits where models may run. Pick another model from the list at the top of the page.',
      }
    case DATA_RETENTION_CODE: {
      // The gateway's sentence (printed above, verbatim) already says what
      // happened and who fixes it. Retrying cannot help — AWS will refuse
      // again until the account changes — so no Try again is offered.
      const refused = failure.refusedModel ?? failure.model ?? ''
      return dataRetentionFailureCopy({
        modelName: refused ? formatModelName(refused) : 'This model',
        route: failure.route,
        alias: failure.route ? failure.model : null,
      })
    }
    case 'policy_unavailable':
      return {
        title: 'Chat is paused for safety',
        hint: isAdmin
          ? 'The gateway has no routing policy it can trust, so it is temporarily refusing every request rather than guessing. Try again shortly, and check the gateway’s database connectivity if it persists.'
          : 'The service is temporarily refusing requests for safety. Try again shortly.',
      }
    // --- Vendor failures. Nothing in the organisation's own configuration is
    // wrong in any of these, so none of them sends anyone to the Models page
    // to change a policy — the key, or the vendor, is the subject.
    case 'provider_rate_limited':
      return {
        title: `${vendor} is rate-limiting your organisation’s key`,
        hint: 'Everyone in your organisation shares this key. Wait a moment before trying again.',
      }
    case 'provider_auth_failed':
      return {
        title: `${vendor} rejected your organisation’s key`,
        hint: isAdmin
          ? 'The key darkplum holds for this vendor is no longer accepted — set it again under Model vendors. Other models still work.'
          : 'An administrator needs to fix this. Other models still work.',
      }
    case 'upstream_error':
    case 'upstream_timeout':
      // One headline for both: 502 and 504 differ in what went wrong at the
      // far end, and in nothing the person reading can act on.
      return {
        title: `${vendor} did not answer`,
        hint: 'Nothing was sent to a different model. Try again, or choose another model.',
      }
    case 'invalid_request':
      // The gateway's own sentence is already scrubbed and is printed above
      // this. Adding to it would blame the vendor for a request shape it never
      // saw — this refusal happens before anything leaves.
      return { title: 'That request could not be sent', hint: '' }
    case 'unsupported_parameter':
      return {
        title: 'That request used an unsupported option',
        hint: failure.param
          ? `This request used ‘${failure.param}’, which darkplum does not support yet.`
          : 'This request used an option darkplum does not support yet.',
      }
    default:
      return {
        title: 'The message was not sent',
        hint: 'Nothing was delivered to the model. Try again.',
      }
  }
}

const FailureNotice = ({
  failure,
  isAdmin,
  onRetry,
}: {
  failure: ChatFailure
  isAdmin: boolean
  onRetry?: () => void
}) => {
  const { title, hint } = failureCopy(failure, isAdmin)
  const modelGone = failure.modelGone ?? false
  // A shared key that has just been rate-limited is made worse by everyone
  // retrying at once, so the way back is held shut for the interval the vendor
  // asked for — or a short fixed one when it named none.
  const cooldown =
    failure.code === 'provider_rate_limited'
      ? (failure.retryAfterSeconds ?? RETRY_AFTER_DEFAULT_SECONDS)
      : 0
  const [waiting, setWaiting] = useState(cooldown)
  // Each send makes a new failure object, so a second refusal restarts the
  // wait rather than inheriting whatever was left of the first.
  useEffect(() => {
    setWaiting(cooldown)
  }, [failure, cooldown])
  useEffect(() => {
    if (waiting <= 0) return
    const timer = window.setTimeout(() => setWaiting((left) => left - 1), 1000)
    return () => window.clearTimeout(timer)
  }, [waiting])
  // Fixed in the catalogue: the model is missing from it, or switched off in it.
  // Fixed on the alias, under Admin > Routing: a routed pick with no usable
  // route to fall back to.
  const aliasRelated = isRouteTargetRefusal(failure.code, failure.message)
  const catalogueRelated =
    !aliasRelated && (failure.code === 'model_not_found' || failure.code === 'model_not_enabled')
  // Fixed in the organisation's routing policy, which is its own tab on the
  // Models page — the model is enabled, the policy is what refuses it.
  const orgPolicyRelated = failure.code === 'model_residency_violation'
  // A per-principal refusal is fixed where per-principal policy is written, not
  // on the Models page: the model itself is enabled and allowed for the org.
  const principalPolicyRelated = failure.code === 'model_not_allowed'
  // Fixed by setting the organisation's key again, which is the vendor block
  // of the Models page rather than the catalogue or any policy.
  const vendorKeyRelated = failure.code === 'provider_auth_failed'
  // Fixed in AWS, not here: a retry is refused again until the account's
  // Bedrock data-retention mode changes. The Models page names the mode.
  const awsOptInRelated = failure.code === DATA_RETENTION_CODE

  return (
    <div className={styles.failure} role="alert">
      <div className={styles.failureHead}>
        <svg
          className={styles.failureIcon}
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <circle cx="12" cy="12" r="10" />
          <line x1="12" y1="8" x2="12" y2="13" />
          <line x1="12" y1="16" x2="12.01" y2="16" />
        </svg>
        <span className={styles.failureTitle}>{title}</span>
      </div>
      <p className={styles.failureMessage}>{failure.message}</p>
      {/* A confirmed disappearance overrides the per-role advice: telling an
          admin to enable a model the gateway no longer discovers is a dead end.
          Some refusals say everything in the gateway's own sentence above and
          have no next step to add; those print no hint rather than padding. */}
      {(modelGone || hint) && (
        <p className={styles.failureHint}>
          {modelGone
            ? 'This model is no longer available. Choose another model at the top of the page, then try again.'
            : hint}
        </p>
      )}
      <div className={styles.failureActions}>
        {onRetry && !awsOptInRelated && (
          /* aria-disabled rather than disabled: pressing Try again into a rate
             limit refuses the send again and starts a fresh wait, and a real
             `disabled` at that moment would drop the focus the person was
             holding onto the body. This keeps it, and keeps the control
             reachable so its name can be read. */
          <button
            type="button"
            className={styles.failureBtn}
            onClick={() => {
              if (waiting <= 0) onRetry()
            }}
            aria-disabled={waiting > 0 || undefined}
          >
            Try again
            {/* The button's own name updates as this ticks, which is what a
                screen reader reads on focus. It is aria-live="off" because the
                card around it is role="alert": without this, every second of
                the wait would be announced over whatever else is being read. */}
            {waiting > 0 && (
              <span className={styles.failureCountdown} aria-live="off">
                {' in '}
                {formatRetryCountdown(waiting)}
              </span>
            )}
          </button>
        )}
        {isAdmin && aliasRelated && (
          <Link to="/admin/routing" className={styles.failureLink}>
            Open Routing
          </Link>
        )}
        {isAdmin && catalogueRelated && (
          <Link to="/admin/models" className={styles.failureLink}>
            Open Models
          </Link>
        )}
        {isAdmin && orgPolicyRelated && (
          <Link to="/admin/models?tab=policy" className={styles.failureLink}>
            Open routing policy
          </Link>
        )}
        {isAdmin && principalPolicyRelated && (
          <Link to="/admin/users" className={styles.failureLink}>
            Open Users &amp; Keys
          </Link>
        )}
        {isAdmin && vendorKeyRelated && (
          <Link to="/admin/models" className={styles.failureLink}>
            Open Model vendors
          </Link>
        )}
        {isAdmin && awsOptInRelated && (
          <Link to="/admin/models" className={styles.failureLink}>
            Open Models
          </Link>
        )}
        {failure.model && (
          <code className={styles.failureModel} title="Model requested">{failure.model}</code>
        )}
      </div>
    </div>
  )
}

const remarkPlugins = [remarkGfm]
const rehypePlugins = [rehypeHighlight]

const mdComponents = {
  pre: CodeBlock,
  code({ className, children, ...props }: React.HTMLAttributes<HTMLElement> & { children?: React.ReactNode }) {
    const isInline = !className
    if (isInline) {
      return <code className={styles.inlineCode} {...props}>{children}</code>
    }
    return <code className={className} {...props}>{children}</code>
  },
  a({ children, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { children?: React.ReactNode }) {
    return <a className={styles.link} target="_blank" rel="noopener noreferrer" {...props}>{children}</a>
  },
  table({ children, ...props }: React.TableHTMLAttributes<HTMLTableElement> & { children?: React.ReactNode }) {
    return (
      <div className={styles.tableWrap}>
        <table className={styles.mdTable} {...props}>{children}</table>
      </div>
    )
  },
}

const UserBubble = memo(({ content }: { content: string }) => (
  <div className={`${styles.message} ${styles.user}`}>
    <div className={styles.bubble}>{content}</div>
  </div>
))
UserBubble.displayName = 'UserBubble'

const ThinkingIndicator = () => (
  <div className={styles.thinking}>
    <span className={styles.thinkingLabel}>Thinking</span>
    <span className={styles.thinkingDots} aria-hidden="true">
      <span className={styles.thinkingDot} />
      <span className={styles.thinkingDot} />
      <span className={styles.thinkingDot} />
    </span>
  </div>
)

/** Which model answered, and — for a routed alias — the route the administrator
 *  wrote that it came through. Never a guess: both come from the gateway's own
 *  response headers, and the model is re-read from history on reload. */
const Provenance = ({
  model,
  route,
  reason,
  fellBack,
  backendClass,
  provider,
  reasonId,
  reasonOpen,
  onToggleReason,
}: {
  model: string
  route?: string | null
  /** Already in plain language. Empty when the gateway sent no reason. */
  reason: string
  /** The picked route could not answer and the alias's default route did.
   *  Said on the line itself rather than behind "Why this route?": the reply
   *  came from a model the route was not written for. */
  fellBack: boolean
  /** Where this reply ran, from the gateway's header. Absent after a reload:
   *  history stores the model, not the class. */
  backendClass?: BackendClass | null
  provider?: string | null
  reasonId: string
  reasonOpen: boolean
  onToggleReason: () => void
}) => {
  /**
   * Where it went, said only when the gateway told us and only when it left
   * the account.
   *
   * The ordinary case gets nothing: a note under every reply saying it ran
   * where it always runs is noise, and noise is what makes the one reply that
   * went somewhere else invisible. A reloaded conversation carries no class at
   * all, so this is absent rather than guessed — the alternative is a line
   * that reassures on Tuesday and is wrong on Wednesday.
   */
  const egress = backendClass !== null && backendClass !== undefined && isEgress(displayClassOf({ backend_class: backendClass }))
  const vendor = egress ? vendorLabel(provider) : ''

  const spoken = [
    `Answered by ${formatModelName(model)}`,
    egress ? `sent to ${vendor}, which is outside your cloud account` : '',
    fellBack ? reason : '',
    route ? `route ${route}` : '',
  ]
    .filter(Boolean)
    .join('. ')

  return (
    <span className={styles.provenance}>
      <span className={styles.provenanceLine}>
        <span aria-hidden="true">
          answered by <span className={styles.provenanceId} title={model}>{formatModelName(model)}</span>
          {egress && (
            <span
              className={styles.provenanceEgress}
              title={`This message was sent to ${vendor}'s API using your organisation's key, and left your cloud account.`}
            >
              <EgressGlyph />
              sent to {vendor}
            </span>
          )}
          {fellBack && ` · ${reason.charAt(0).toLowerCase()}${reason.slice(1)}`}
          {route && (
            <>
              {' · route '}
              <span className={styles.provenanceId}>{route}</span>
            </>
          )}
        </span>
        <span className={styles.srOnly}>{spoken}</span>
      </span>
      {reason && !fellBack && (
        <button
          type="button"
          className={styles.reasonToggle}
          onClick={onToggleReason}
          aria-expanded={reasonOpen}
          aria-controls={reasonId}
        >
          {reasonOpen ? 'Hide why' : 'Why this route?'}
          <svg
            className={`${styles.reasonChevron} ${reasonOpen ? styles.reasonChevronOpen : ''}`}
            width="12"
            height="12"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <polyline points="6 9 12 15 18 9" />
          </svg>
        </button>
      )}
    </span>
  )
}

/** One tool call and its result, paired by call_id. A call without a
 *  result yet is in flight. */
interface ToolExchange {
  call: ToolEvent
  result?: ToolEvent
}

function pairToolEvents(events: ToolEvent[]): ToolExchange[] {
  const out: ToolExchange[] = []
  const byId = new Map<string, ToolExchange>()
  for (const ev of events) {
    if (ev.type === 'tool_call') {
      const ex: ToolExchange = { call: ev }
      out.push(ex)
      byId.set(ev.call_id, ex)
    } else {
      const ex = byId.get(ev.call_id)
      if (ex) ex.result = ev
      else out.push({ call: { ...ev, type: 'tool_call' }, result: ev })
    }
  }
  return out
}

function formatArguments(args: unknown): string {
  if (args === undefined || args === null) return ''
  if (typeof args === 'string') return args
  try {
    const text = JSON.stringify(args, null, 2)
    return text === '{}' ? '' : text
  } catch {
    return ''
  }
}

/** "A", "A and B", "A, B and C". */
function joinNames(names: string[]): string {
  if (names.length <= 1) return names.join('')
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`
}

function formatDuration(ms?: number): string {
  if (!ms) return ''
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`
}

/** What the api did on the model's behalf before (or while) it answered:
 *  each call, where it went, and what came back. Shown above the reply so
 *  the words below are read knowing what they rest on. */
const ToolActivity = ({ events, streaming }: { events: ToolEvent[]; streaming: boolean }) => {
  const exchanges = pairToolEvents(events)
  if (exchanges.length === 0) return null
  return (
    <ul className={styles.toolActivity} aria-label="Tool calls made for this reply">
      {exchanges.map((ex) => {
        const pending = !ex.result
        const failed = Boolean(ex.result?.is_error)
        const args = formatArguments(ex.call.arguments)
        const where = ex.call.server_name ? `${ex.call.server_name} · ${ex.call.tool}` : ex.call.tool
        const duration = formatDuration(ex.result?.duration_ms)
        const status = pending
          ? streaming
            ? 'Running…'
            : 'No result'
          : failed
            ? 'Failed'
            : duration
              ? `Done in ${duration}`
              : 'Done'
        return (
          <li key={ex.call.call_id}>
            <details className={styles.toolCall}>
              <summary className={styles.toolCallHead}>
                <span
                  className={`${styles.toolStatus} ${pending ? styles.toolStatusPending : failed ? styles.toolStatusError : styles.toolStatusOk}`}
                  aria-hidden="true"
                />
                <span className={styles.toolGlyph}>
                  <KindGlyph kind="tool" size={13} />
                </span>
                <span className={styles.toolCallName}>{where}</span>
                <span className={styles.toolCallMeta}>{status}</span>
                <svg className={styles.toolChevron} width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <polyline points="6 9 12 15 18 9" />
                </svg>
              </summary>
            <div className={styles.toolDetails}>
              {args && (
                <>
                  <div className={styles.toolDetailLabel}>Arguments</div>
                  <pre className={styles.toolPre}>{args}</pre>
                </>
              )}
              {ex.result && (
                <>
                  <div className={styles.toolDetailLabel}>
                    {failed ? 'Error' : 'Result'}
                    {ex.result.truncated ? ' (truncated)' : ''}
                  </div>
                  <pre className={styles.toolPre}>{ex.result.content || '(empty)'}</pre>
                </>
              )}
              </div>
            </details>
          </li>
        )
      })}
    </ul>
  )
}

const AssistantBubble = memo(({
  content,
  model,
  route,
  routerReason,
  backendClass,
  provider,
  toolEvents,
  libraryUsed,
  libraryNames,
  isStreaming,
  isLast,
  onRegenerate,
}: {
  content: string
  /** The model the server recorded as having produced this reply. A thread can
   *  span several, so the answer says which one it came from. */
  model?: string | null
  /** Routed aliases: the route this reply came through, and why it was chosen.
   *  Known for the turn that just arrived; history keeps the model only. */
  route?: string | null
  routerReason?: string | null
  /** Where it ran, and who served it. Same lifetime as `route`. */
  backendClass?: BackendClass | null
  provider?: string | null
  /** Tool calls made for this reply (stored with it), and the library items
   *  the api attached to the turn (header only; gone after a reload). */
  toolEvents?: ToolEvent[] | null
  libraryUsed?: string[] | null
  libraryNames?: Map<string, string>
  isStreaming: boolean
  isLast: boolean
  onRegenerate?: () => void
}) => {
  const reasonId = useId()
  const used = libraryUsed ?? []
  const usedNames = libraryNames ? used.map((id) => libraryNames.get(id) ?? 'an item that is no longer available') : []
  const usedText = libraryNames
    ? joinNames(usedNames)
    : used.length === 1
      ? '1 library item'
      : `${used.length} library items`
  const hasTools = Boolean(toolEvents && toolEvents.length > 0)
  // Closed by default: the line under a reply is a footnote, and the reason is
  // one tap away rather than a hover only a mouse can find.
  const [reasonOpen, setReasonOpen] = useState(false)
  const reason = routerReasonText(routerReason)

  return (
    <div className={`${styles.message} ${styles.assistant}`}>
      <div className={styles.bubble}>
        {hasTools && <ToolActivity events={toolEvents ?? []} streaming={isStreaming} />}
        {isStreaming && !content ? (
          <ThinkingIndicator />
        ) : (
          <>
            <ReactMarkdown
              remarkPlugins={remarkPlugins}
              rehypePlugins={rehypePlugins}
              components={mdComponents}
            >
              {content}
            </ReactMarkdown>
            {isStreaming && <span className={styles.cursor} aria-hidden="true" />}
            {!isStreaming && (content || hasTools) && (
              <div className={styles.footer}>
                <div className={styles.actions}>
                  <CopyButton value={content} label="Copy" />
                  {used.length > 0 && (
                    <span className={styles.usedLine} title="Library items attached to this turn">
                      Used {usedText}
                    </span>
                  )}
                  {model && (
                    <Provenance
                      model={model}
                      route={route}
                      reason={reason}
                      fellBack={routeFallbackOf(routerReason) !== null}
                      backendClass={backendClass}
                      provider={provider}
                      reasonId={reasonId}
                      reasonOpen={reasonOpen}
                      onToggleReason={() => setReasonOpen((prev) => !prev)}
                    />
                  )}
                  {isLast && onRegenerate && (
                    <button className={styles.regenerate} onClick={onRegenerate} type="button">
                      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                        <path d="M1 4v5h5" />
                        <path d="M3.51 10a6 6 0 1 0 .49-5L1 9" />
                      </svg>
                      Regenerate
                    </button>
                  )}
                </div>
                {model && reason && reasonOpen && (
                  <p className={styles.provenanceReason} id={reasonId}>{reason}</p>
                )}
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
})
AssistantBubble.displayName = 'AssistantBubble'

export function MessageList({
  messages,
  streaming,
  onRegenerate,
  failure,
  isAdmin,
  onRetry,
  unsavedFrom,
  libraryNames,
}: Props) {
  const bottomRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const [userScrolled, setUserScrolled] = useState(false)

  useEffect(() => {
    if (!userScrolled) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
    }
  }, [messages, userScrolled])

  // A failure always scrolls into view — it is the answer to what the person
  // just did, even if they had scrolled up.
  useEffect(() => {
    if (failure) bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [failure])

  const handleScroll = () => {
    const el = containerRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    setUserScrolled(!atBottom)
  }

  if (messages.length === 0) {
    return null
  }

  return (
    <div className={styles.container} ref={containerRef} onScroll={handleScroll}>
      <div className={styles.inner}>
        {messages.map((msg, i) => {
          const isLast = i === messages.length - 1
          const unsaved = unsavedFrom !== null && unsavedFrom !== undefined && i === unsavedFrom
          const bubble =
            msg.role === 'user' ? (
              <UserBubble key={i} content={msg.content} />
            ) : (
              <AssistantBubble
                key={i}
                content={msg.content}
                model={msg.model}
                route={msg.route}
                routerReason={msg.routerReason}
                backendClass={msg.backendClass}
                provider={msg.provider}
                toolEvents={msg.toolEvents}
                libraryUsed={msg.libraryUsed}
                libraryNames={libraryNames}
                isStreaming={isLast && streaming}
                isLast={isLast}
                onRegenerate={onRegenerate}
              />
            )
          if (!unsaved) return bubble
          return (
            <div key={i} className={styles.turn}>
              {bubble}
              <UnsavedNotice />
            </div>
          )
        })}
        {failure && (
          <FailureNotice failure={failure} isAdmin={isAdmin ?? false} onRetry={onRetry} />
        )}
        <div ref={bottomRef} />
      </div>
    </div>
  )
}
