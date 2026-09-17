import { memo, useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import type { ChatMessage } from '../types'
import { CodeBlock } from '../components/ui/CodeBlock'
import { CopyButton } from '../components/ui/CopyButton'
import styles from './MessageList.module.css'

interface Props {
  messages: ChatMessage[]
  streaming: boolean
  onRegenerate?: () => void
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

const AssistantBubble = memo(({
  content,
  isStreaming,
  isLast,
  onRegenerate,
}: {
  content: string
  isStreaming: boolean
  isLast: boolean
  onRegenerate?: () => void
}) => (
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
            <div className={styles.actions}>
              <CopyButton value={content} label="Copy" />
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
          )}
        </>
      )}
    </div>
  </div>
))
AssistantBubble.displayName = 'AssistantBubble'

export function MessageList({ messages, streaming, onRegenerate }: Props) {
  const bottomRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const [userScrolled, setUserScrolled] = useState(false)

  useEffect(() => {
    if (!userScrolled) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
    }
  }, [messages, userScrolled])

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
          if (msg.role === 'user') {
            return <UserBubble key={i} content={msg.content} />
          }
          return (
            <AssistantBubble
              key={i}
              content={msg.content}
              isStreaming={isLast && streaming}
              isLast={isLast}
              onRegenerate={onRegenerate}
            />
          )
        })}
        <div ref={bottomRef} />
      </div>
    </div>
  )
}
