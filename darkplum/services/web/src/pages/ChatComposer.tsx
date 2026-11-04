import { useEffect, useId, useRef, useState } from 'react'
import type { LibraryItem, LibraryKind } from '../types'
import { KindGlyph } from '../components/ui/KindGlyph'
import { KIND_LABEL } from './library/libraryText'
import { LibraryPicker } from './LibraryPicker'
import { ACCEPT, formatSize, typeLabel, type PendingAttachment } from '../lib/attachments'
import styles from './ChatComposer.module.css'

/** A file that could not be attached, kept in the tray by name until removed:
 *  refused where it was dropped, never in a toast that scrolls away. */
export interface RejectedAttachment {
  name: string
  error: string
}

/** Everything the composer needs to take images and files. */
export interface ComposerAttachments {
  items: PendingAttachment[]
  rejected: RejectedAttachment[]
  /** Reading, shrinking or encoding something still: Send waits. */
  busy: boolean
  onAdd: (files: File[]) => void
  onRemove: (index: number) => void
  onDismissRejected: (index: number) => void
  /** Where the bytes go, in one line: inside the cloud account, or to a vendor. */
  residency?: string
  /** Set when the picked model cannot take images or files: the control is
   *  off and says why, the line below says it again while files are staged. */
  disabledReason?: string
}

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
  onSend: (text: string, attachments: PendingAttachment[]) => void
  onStop: () => void
  streaming: boolean
  disabled?: boolean
  /** Shown in place of the disclaimer while the composer is unusable. */
  notice?: ComposerNotice
  library?: ComposerLibrary
  attachments?: ComposerAttachments
}

export function ChatComposer({ onSend, onStop, streaming, disabled, notice, library, attachments }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const pickerWrapRef = useRef<HTMLDivElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const noticeId = useId()
  const showNotice = Boolean(disabled && notice)
  const [pickerOpen, setPickerOpen] = useState(false)
  // Re-rendered on input so the Send title can say why it is held.
  const [draftText, setDraftText] = useState('')
  const files = attachments?.items ?? []
  const rejected = attachments?.rejected ?? []
  const hasDocument = files.some((f) => f.kind === 'file')

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

  // Why Send is held, in one sentence, or null when it may go.
  const holdReason = (text: string): string | null => {
    if (showNotice && notice) return notice.text
    if (attachments?.busy) return 'Wait for attachments to finish loading.'
    if (attachments?.disabledReason && files.length > 0) return attachments.disabledReason
    if (rejected.length === 1) return `Remove ${rejected[0].name} to send.`
    if (rejected.length > 1) return 'Remove the files marked in red to send.'
    // Bedrock reads a document only alongside words; an image needs none.
    if (hasDocument && !text) return 'Add a message to send with a document.'
    return null
  }

  const submit = () => {
    const el = textareaRef.current
    if (!el) return
    const text = el.value.trim()
    if (streaming || disabled) return
    if (!text && files.length === 0) return
    if (holdReason(text) !== null) return
    el.value = ''
    el.style.height = 'auto'
    setDraftText('')
    onSend(text, files)
  }

  // Screenshots and copied images attach; text copied from a page that also
  // carries an image stays text.
  const handlePaste = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    if (!attachments || attachments.disabledReason || streaming || disabled) return
    const dt = e.clipboardData
    if (dt.files.length === 0 || dt.types.includes('text/plain')) return
    e.preventDefault()
    attachments.onAdd(Array.from(dt.files))
  }

  const handlePick = (e: React.ChangeEvent<HTMLInputElement>) => {
    const list = e.target.files
    if (list && list.length > 0) attachments?.onAdd(Array.from(list))
    // The same file can be chosen again after being removed.
    e.target.value = ''
  }

  const closePicker = () => {
    setPickerOpen(false)
    textareaRef.current?.focus()
  }

  const chips = library?.attached ?? []
  const sendTitle = holdReason(draftText) ?? 'Send message'

  return (
    <div className={styles.wrapper}>
      <div className={`${styles.composer} ${disabled ? styles.composerDisabled : ''}`}>
        {(files.length > 0 || rejected.length > 0) && (
          <div className={styles.tray} role="list" aria-label="Attachments">
            {files.map((f, i) => (
              <div
                key={`${f.name}-${i}`}
                role="listitem"
                className={`${styles.tile} ${f.kind === 'image' ? styles.tileImage : styles.tileFile}`}
                title={f.resizedTo ? `${f.name} (resized to ${f.resizedTo.width} × ${f.resizedTo.height} to fit)` : f.name}
              >
                {f.kind === 'image' && f.previewUrl ? (
                  <img src={f.previewUrl} alt={f.name} />
                ) : (
                  <>
                    <span className={styles.tileName}>{f.name}</span>
                    <span className={styles.tileMeta}>
                      <span className={styles.typeBadge}>{typeLabel(f.mediaType)}</span>
                      <span>{formatSize(f.size)}</span>
                    </span>
                  </>
                )}
                <button
                  type="button"
                  className={styles.tileRemove}
                  onClick={() => attachments?.onRemove(i)}
                  aria-label={`Remove ${f.name}`}
                  disabled={streaming}
                >
                  <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
                    <line x1="18" y1="6" x2="6" y2="18" />
                    <line x1="6" y1="6" x2="18" y2="18" />
                  </svg>
                </button>
              </div>
            ))}
            {rejected.map((r, i) => (
              <div key={`rejected-${r.name}-${i}`} role="listitem" className={`${styles.tile} ${styles.tileFile} ${styles.tileError}`} title={r.error}>
                <span className={styles.tileName}>{r.name}</span>
                <span className={`${styles.tileMeta} ${styles.tileErrorLabel}`}>
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                    <circle cx="12" cy="12" r="10" />
                    <line x1="12" y1="8" x2="12" y2="12" />
                    <line x1="12" y1="16" x2="12.01" y2="16" />
                  </svg>
                  <span>Not added</span>
                </span>
                <button
                  type="button"
                  className={styles.tileRemove}
                  onClick={() => attachments?.onDismissRejected(i)}
                  aria-label={`Remove ${r.name}`}
                >
                  <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
                    <line x1="18" y1="6" x2="6" y2="18" />
                    <line x1="6" y1="6" x2="18" y2="18" />
                  </svg>
                </button>
              </div>
            ))}
          </div>
        )}
        {rejected.length > 0 && (
          <ul className={styles.trayErrors} aria-live="polite">
            {rejected.slice(0, 3).map((r, i) => (
              <li key={`err-${i}`}>{r.error}</li>
            ))}
            {rejected.length > 3 && <li>and {rejected.length - 3} more.</li>}
          </ul>
        )}
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
          {attachments && (
            <>
              <input
                ref={fileInputRef}
                type="file"
                multiple
                accept={ACCEPT}
                className={styles.fileInput}
                tabIndex={-1}
                aria-hidden="true"
                onChange={handlePick}
              />
              <button
                type="button"
                className={styles.attachBtn}
                onClick={() => fileInputRef.current?.click()}
                aria-label="Add files or images"
                title={attachments.disabledReason ?? 'Add files or images'}
                disabled={streaming || disabled || Boolean(attachments.disabledReason)}
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48" />
                </svg>
              </button>
            </>
          )}
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
            onInput={(e) => {
              resize()
              setDraftText(e.currentTarget.value.trim())
            }}
            onKeyDown={handleKeyDown}
            onPaste={handlePaste}
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
              title={sendTitle}
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
        <p className={styles.disclaimer}>
          {files.length > 0 && (attachments?.disabledReason || attachments?.residency) ? (
            <span className={styles.residency}>{attachments.disabledReason ?? attachments.residency} </span>
          ) : null}
          AI responses may be inaccurate. Always verify important information.
        </p>
      )}
    </div>
  )
}
