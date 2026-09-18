import { memo, useEffect, useId, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import type { ChatMessage } from '../types'
import { formatModelName } from '../lib/formatModelName'
import { routerReasonText } from '../lib/modelRouting'
import { CodeBlock } from '../components/ui/CodeBlock'
import { CopyButton } from '../components/ui/CopyButton'
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
}

interface Props {
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

/** Headline and next step for each routing refusal the gateway can return.
 *  When a re-read of the list has confirmed the model is gone, the next step is
 *  the same for everyone — choose another one — so it replaces the per-role
 *  advice, which would otherwise point at a model that no longer exists. */
function failureCopy(
  code: string | undefined,
  isAdmin: boolean,
): { title: string; hint: string } {
  switch (code) {
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
    case 'model_residency_violation':
      return {
        title: 'Blocked by the routing policy',
        hint: isAdmin
          ? 'The policy allows self-hosted models only. Pick a self-hosted model, or change the routing policy on the Models page.'
          : 'Your organization allows self-hosted models only. Pick a self-hosted model from the list.',
      }
    case 'policy_unavailable':
      return {
        title: 'Chat is paused for safety',
        hint: isAdmin
          ? 'The gateway has no routing policy it can trust, so it is temporarily refusing every request rather than guessing. Try again shortly, and check the gateway’s database connectivity if it persists.'
          : 'The service is temporarily refusing requests for safety. Try again shortly.',
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
  const { title, hint } = failureCopy(failure.code, isAdmin)
  const modelGone = failure.modelGone ?? false
  const policyRelated =
    failure.code === 'model_not_found' ||
    failure.code === 'model_not_enabled' ||
    failure.code === 'model_residency_violation'

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
          admin to enable a model the gateway no longer discovers is a dead end. */}
      <p className={styles.failureHint}>
        {modelGone
          ? 'This model is no longer available. Choose another model at the top of the page, then try again.'
          : hint}
      </p>
      <div className={styles.failureActions}>
        {onRetry && (
          <button type="button" className={styles.failureBtn} onClick={onRetry}>
            Try again
          </button>
        )}
        {isAdmin && policyRelated && (
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
  rerouted,
  reasonId,
  reasonOpen,
  onToggleReason,
}: {
  model: string
  route?: string | null
  /** Already in plain language. Empty when the gateway sent no reason. */
  reason: string
  rerouted?: boolean
  reasonId: string
  reasonOpen: boolean
  onToggleReason: () => void
}) => {
  const spoken = [
    `Answered by ${formatModelName(model)}`,
    route ? `route ${route}` : '',
    rerouted ? 'the model this conversation had been using was no longer available' : '',
  ]
    .filter(Boolean)
    .join('. ')

  return (
    <span className={styles.provenance}>
      <span className={styles.provenanceLine}>
        <span aria-hidden="true">
          answered by <span className={styles.provenanceId} title={model}>{formatModelName(model)}</span>
          {route && (
            <>
              {' · route '}
              <span className={styles.provenanceId}>{route}</span>
            </>
          )}
          {rerouted && (
            <span
              className={styles.provenanceNote}
              title="The model this conversation had been using is no longer available, so the alias chose again."
            >
              re-routed
            </span>
          )}
        </span>
        <span className={styles.srOnly}>{spoken}</span>
      </span>
      {reason && (
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

const AssistantBubble = memo(({
  content,
  model,
  route,
  routerReason,
  rerouted,
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
  rerouted?: boolean
  isStreaming: boolean
  isLast: boolean
  onRegenerate?: () => void
}) => {
  const reasonId = useId()
  // Closed by default: the line under a reply is a footnote, and the reason is
  // one tap away rather than a hover only a mouse can find.
  const [reasonOpen, setReasonOpen] = useState(false)
  const reason = routerReasonText(routerReason)

  return (
    <div className={`${styles.message} ${styles.assistant}`}>
      <div className={styles.bubble}>
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
            {!isStreaming && content && (
              <div className={styles.footer}>
                <div className={styles.actions}>
                  <CopyButton value={content} label="Copy" />
                  {model && (
                    <Provenance
                      model={model}
                      route={route}
                      reason={reason}
                      rerouted={rerouted}
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
                rerouted={msg.rerouted}
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
