import { useEffect, useId, useRef } from 'react'
import styles from './ChatComposer.module.css'

/** Why the composer is off, and what the reader can do about it. */
export interface ComposerNotice {
  /** Plain sentence. Used wherever only text fits, e.g. the send tooltip. */
  text: string
  /** The same sentence with an inline link, when there is somewhere to go. */
  content?: React.ReactNode
  /** An action on the same line, e.g. retrying a failed model load. */
  action?: { label: string; onClick: () => void }
}

interface Props {
  onSend: (text: string) => void
  onStop: () => void
  streaming: boolean
  disabled?: boolean
  /** Shown in place of the disclaimer while the composer is unusable. */
  notice?: ComposerNotice
}

export function ChatComposer({ onSend, onStop, streaming, disabled, notice }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const noticeId = useId()
  const showNotice = Boolean(disabled && notice)

  // Auto-resize textarea
  const resize = () => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`
  }

  useEffect(() => {
    resize()
  }, [])

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  const submit = () => {
    const el = textareaRef.current
    if (!el) return
    const text = el.value.trim()
    if (!text || streaming || disabled) return
    el.value = ''
    el.style.height = 'auto'
    onSend(text)
  }

  return (
    <div className={styles.wrapper}>
      <div className={`${styles.composer} ${disabled ? styles.composerDisabled : ''}`}>
        <textarea
          ref={textareaRef}
          className={styles.textarea}
          /* No placeholder while off: the line below already says why, and the
             same sentence twice reads as a rendering bug. */
          placeholder={disabled ? undefined : 'Message darkplum...'}
          aria-label="Message"
          aria-describedby={showNotice ? noticeId : undefined}
          rows={1}
          onInput={resize}
          onKeyDown={handleKeyDown}
          disabled={streaming || disabled}
        />

        {streaming ? (
          <button className={styles.stopBtn} onClick={onStop} title="Stop generating" aria-label="Stop generating">
            <svg width="14" height="14" viewBox="0 0 14 14" fill="currentColor" aria-hidden="true">
              <rect x="2" y="2" width="10" height="10" rx="2" />
            </svg>
          </button>
        ) : (
          <button
            className={styles.sendBtn}
            onClick={submit}
            disabled={disabled}
            title={showNotice && notice ? notice.text : 'Send message'}
            aria-label="Send message"
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <line x1="8" y1="14" x2="8" y2="2" />
              <polyline points="3,7 8,2 13,7" />
            </svg>
          </button>
        )}
      </div>
      {showNotice && notice ? (
        <p id={noticeId} className={styles.reason}>
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <circle cx="12" cy="12" r="10" />
            <line x1="12" y1="16" x2="12" y2="11" />
            <line x1="12" y1="8" x2="12.01" y2="8" />
          </svg>
          <span>{notice.content ?? notice.text}</span>
          {notice.action && (
            <button type="button" className={styles.reasonAction} onClick={notice.action.onClick}>
              {notice.action.label}
            </button>
          )}
        </p>
      ) : (
        <p className={styles.disclaimer}>AI responses may be inaccurate. Always verify important information.</p>
      )}
    </div>
  )
}
