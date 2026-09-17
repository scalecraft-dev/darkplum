import { useEffect, useRef } from 'react'
import styles from './ChatComposer.module.css'

interface Props {
  onSend: (text: string) => void
  onStop: () => void
  streaming: boolean
  disabled?: boolean
}

export function ChatComposer({ onSend, onStop, streaming, disabled }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)

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
      <div className={styles.composer}>
        <textarea
          ref={textareaRef}
          className={styles.textarea}
          placeholder="Message darkplum..."
          rows={1}
          onInput={resize}
          onKeyDown={handleKeyDown}
          disabled={streaming || disabled}
        />

        {streaming ? (
          <button className={styles.stopBtn} onClick={onStop} title="Stop generating">
            <svg width="14" height="14" viewBox="0 0 14 14" fill="currentColor">
              <rect x="2" y="2" width="10" height="10" rx="2" />
            </svg>
          </button>
        ) : (
          <button className={styles.sendBtn} onClick={submit} disabled={disabled} title="Send message">
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <line x1="8" y1="14" x2="8" y2="2" />
              <polyline points="3,7 8,2 13,7" />
            </svg>
          </button>
        )}
      </div>
      <p className={styles.disclaimer}>AI responses may be inaccurate. Always verify important information.</p>
    </div>
  )
}
