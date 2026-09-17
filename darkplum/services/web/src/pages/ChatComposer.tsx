import { useEffect, useId, useRef } from 'react'
import styles from './ChatComposer.module.css'

interface Props {
  onSend: (text: string) => void
  onStop: () => void
  streaming: boolean
  disabled?: boolean
  /** Shown in place of the disclaimer while the composer is unusable. */
  disabledReason?: string
}

export function ChatComposer({ onSend, onStop, streaming, disabled, disabledReason }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const reasonId = useId()
  const showReason = Boolean(disabled && disabledReason)

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
          placeholder={disabled && disabledReason ? disabledReason : 'Message darkplum...'}
          aria-label="Message"
          aria-describedby={showReason ? reasonId : undefined}
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
            title={disabled && disabledReason ? disabledReason : 'Send message'}
            aria-label="Send message"
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <line x1="8" y1="14" x2="8" y2="2" />
              <polyline points="3,7 8,2 13,7" />
            </svg>
          </button>
        )}
      </div>
      {showReason ? (
        <p id={reasonId} className={styles.reason}>
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
            <path d="M7 11V7a5 5 0 0 1 10 0v4" />
          </svg>
          {disabledReason}
        </p>
      ) : (
        <p className={styles.disclaimer}>AI responses may be inaccurate. Always verify important information.</p>
      )}
    </div>
  )
}
