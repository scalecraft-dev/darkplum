import { useEffect, useId, useRef, useState } from 'react'
import type { LibraryItem, LibraryKind } from '../types'
import { KindGlyph } from '../components/ui/KindGlyph'
import { KIND_LABEL } from './library/libraryText'
import { LibraryPicker } from './LibraryPicker'
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

/** One attached library item as the chip row shows it. */
export interface AttachedChip {
  id: string
  name: string
  kind: LibraryKind
  /** The server said this item is gone or no longer shared: the chip stays,
   *  muted, until the person removes it, rather than vanishing in silence. */
  missing?: boolean
}

/** Everything the composer needs to attach library items. Absent on a
 *  deployment or screen that has no library. */
export interface ComposerLibrary {
  attached: AttachedChip[]
  onRemove: (id: string) => void
  onToggle: (item: LibraryItem) => void
  items: LibraryItem[]
  loading: boolean
  error: string | null
  onRefresh: () => void
  /** How many more may be attached. */
  remaining: number
}

interface Props {
  onSend: (text: string) => void
  onStop: () => void
  streaming: boolean
  disabled?: boolean
  /** Shown in place of the disclaimer while the composer is unusable. */
  notice?: ComposerNotice
  library?: ComposerLibrary
}

export function ChatComposer({ onSend, onStop, streaming, disabled, notice, library }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const pickerWrapRef = useRef<HTMLDivElement>(null)
  const noticeId = useId()
  const showNotice = Boolean(disabled && notice)
  const [pickerOpen, setPickerOpen] = useState(false)

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

  // The picker closes on a click anywhere else, and on Escape from within.
  useEffect(() => {
    if (!pickerOpen) return
    const onDown = (e: MouseEvent) => {
      if (pickerWrapRef.current && !pickerWrapRef.current.contains(e.target as Node)) setPickerOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [pickerOpen])

  useEffect(() => {
    if (pickerOpen) library?.onRefresh()
    // Refresh when the menu opens, not on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pickerOpen])

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

  const closePicker = () => {
    setPickerOpen(false)
    textareaRef.current?.focus()
  }

  const chips = library?.attached ?? []

  return (
    <div className={styles.wrapper}>
      <div className={`${styles.composer} ${disabled ? styles.composerDisabled : ''}`}>
        {chips.length > 0 && (
          <div className={styles.chipRow} role="list" aria-label="Attached from your library">
            {chips.map((chip) => (
              <span
                key={chip.id}
                role="listitem"
                className={`${styles.chip} ${chip.missing ? styles.chipMissing : ''}`}
                title={chip.missing ? 'This item was removed or is no longer shared with you. Remove it to send.' : `${KIND_LABEL[chip.kind]}: ${chip.name}`}
              >
                <span className={styles.chipGlyph}>
                  <KindGlyph kind={chip.kind} size={12} />
                </span>
                <span className={styles.chipName}>{chip.name}</span>
                {chip.missing && <span className={styles.chipNote}>No longer available. Remove it to send.</span>}
                <button
                  type="button"
                  className={styles.chipRemove}
                  onClick={() => library?.onRemove(chip.id)}
                  aria-label={`Remove ${chip.name}`}
                  disabled={streaming}
                >
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
                    <line x1="18" y1="6" x2="6" y2="18" />
                    <line x1="6" y1="6" x2="18" y2="18" />
                  </svg>
                </button>
              </span>
            ))}
          </div>
        )}
        <div className={styles.inputRow}>
          {library && (
            <div className={styles.pickerWrap} ref={pickerWrapRef}>
              <button
                type="button"
                className={`${styles.attachBtn} ${pickerOpen ? styles.attachBtnOpen : ''}`}
                onClick={() => setPickerOpen((o) => !o)}
                aria-expanded={pickerOpen}
                aria-haspopup="dialog"
                aria-label="Attach from your library"
                title="Attach a skill, tool or context from your library"
                disabled={streaming}
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                  <line x1="12" y1="5" x2="12" y2="19" />
                  <line x1="5" y1="12" x2="19" y2="12" />
                </svg>
              </button>
              {pickerOpen && (
                <LibraryPicker
                  items={library.items}
                  loading={library.loading}
                  error={library.error}
                  attachedIds={chips.map((c) => c.id)}
                  onToggle={library.onToggle}
                  onClose={closePicker}
                  onRefresh={library.onRefresh}
                  remaining={library.remaining}
                />
              )}
            </div>
          )}
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
