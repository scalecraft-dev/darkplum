import { useEffect, useRef, useState } from 'react'
import type { Conversation } from '../lib/conversations'
import { Button, Modal } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './ConversationList.module.css'

export type ListState = 'loading' | 'ready' | 'error'

interface Props {
  conversations: Conversation[]
  activeId: string | null
  state: ListState
  error: string | null
  /** History from before the move is on its way to the server. */
  importing: boolean
  /** Some of it did not get there, and is still on this device. */
  importFailed: boolean
  onSelect: (id: string) => void
  onNew: () => void
  onDelete: (id: string) => void
  onRename: (id: string, title: string) => void
  onRetry: () => void
}

const SKELETON_WIDTHS = ['82%', '64%', '73%', '56%', '69%']

export function ConversationList({
  conversations,
  activeId,
  state,
  error,
  importing,
  importFailed,
  onSelect,
  onNew,
  onDelete,
  onRename,
  onRetry,
}: Props) {
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [draftTitle, setDraftTitle] = useState('')
  const [pendingDelete, setPendingDelete] = useState<Conversation | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  /** The row buttons, so focus goes back where it came from when renaming ends
   *  — otherwise the input unmounts and keyboard focus falls to the document. */
  const rowRefs = useRef(new Map<string, HTMLButtonElement | null>())

  useEffect(() => {
    if (renamingId) inputRef.current?.select()
  }, [renamingId])

  const startRename = (conv: Conversation) => {
    setRenamingId(conv.id)
    setDraftTitle(conv.title)
  }

  const endRename = (id: string) => {
    setRenamingId(null)
    requestAnimationFrame(() => rowRefs.current.get(id)?.focus())
  }

  const commitRename = () => {
    if (!renamingId) return
    const title = draftTitle.trim()
    if (title) onRename(renamingId, title)
    endRename(renamingId)
  }

  const confirmDelete = () => {
    if (!pendingDelete) return
    onDelete(pendingDelete.id)
    setPendingDelete(null)
  }

  return (
    <div className={styles.panel}>
      <button type="button" className={styles.newBtn} onClick={onNew}>
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
          <line x1="8" y1="3" x2="8" y2="13" />
          <line x1="3" y1="8" x2="13" y2="8" />
        </svg>
        New chat
      </button>

      {importing && (
        <p className={styles.notice} role="status">
          Moving your saved conversations to the server…
        </p>
      )}
      {!importing && importFailed && (
        <p className={styles.noticeWarn} role="status">
          Some past conversations could not be moved to the server yet; they are still on this
          device.
        </p>
      )}

      {state === 'loading' && (
        <div className={styles.skeletons} role="status" aria-live="polite">
          <span className={styles.srOnly}>Loading your conversations</span>
          {SKELETON_WIDTHS.map((width, i) => (
            <Skeleton key={i} width={width} height="17px" borderRadius="var(--radius-sm)" />
          ))}
        </div>
      )}

      {state === 'error' && (
        <div className={styles.panelError} role="alert">
          <p className={styles.panelErrorTitle}>Conversations didn't load</p>
          <p className={styles.panelErrorBody}>{error}</p>
          <button type="button" className={styles.panelErrorBtn} onClick={onRetry}>
            Try again
          </button>
        </div>
      )}

      {state === 'ready' && conversations.length === 0 && (
        <div className={styles.empty}>
          <p className={styles.emptyTitle}>No conversations yet</p>
          <p className={styles.emptyBody}>
            Send a message and it is saved to your account, ready on any browser you sign in
            from.
          </p>
        </div>
      )}

      {conversations.length > 0 && (
        <ul className={styles.list} role="list">
          {conversations.map((c) => {
            const isRenaming = renamingId === c.id
            return (
              <li
                key={c.id}
                className={`${styles.itemRow} ${c.id === activeId ? styles.active : ''}`}
              >
                {isRenaming ? (
                  <input
                    ref={inputRef}
                    className={styles.renameInput}
                    value={draftTitle}
                    aria-label="Conversation title"
                    maxLength={200}
                    onChange={(e) => setDraftTitle(e.target.value)}
                    onBlur={commitRename}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault()
                        commitRename()
                      } else if (e.key === 'Escape') {
                        e.preventDefault()
                        endRename(c.id)
                      }
                    }}
                    autoFocus
                  />
                ) : (
                  <>
                    <button
                      type="button"
                      ref={(el) => rowRefs.current.set(c.id, el)}
                      className={styles.itemBtn}
                      onClick={() => onSelect(c.id)}
                      onDoubleClick={() => startRename(c)}
                      aria-current={c.id === activeId ? 'true' : undefined}
                    >
                      <span className={styles.title}>{c.title}</span>
                    </button>
                    <button
                      type="button"
                      className={styles.rowBtn}
                      onClick={() => startRename(c)}
                      aria-label={`Rename conversation: ${c.title}`}
                    >
                      <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                        <path d="M11.2 2.3a1.5 1.5 0 0 1 2.1 2.1L5.6 12.1l-2.8.7.7-2.8z" />
                      </svg>
                    </button>
                    <button
                      type="button"
                      className={`${styles.rowBtn} ${styles.deleteBtn}`}
                      onClick={() => setPendingDelete(c)}
                      aria-label={`Delete conversation: ${c.title}`}
                    >
                      <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                        <line x1="4" y1="4" x2="12" y2="12" />
                        <line x1="12" y1="4" x2="4" y2="12" />
                      </svg>
                    </button>
                  </>
                )}
              </li>
            )
          })}
        </ul>
      )}

      <Modal
        open={pendingDelete !== null}
        onClose={() => setPendingDelete(null)}
        title="Delete this conversation?"
        description={
          pendingDelete
            ? `“${pendingDelete.title}” and every message in it are deleted for good. There is no undo.`
            : undefined
        }
      >
        <div className={styles.modalFooter}>
          <Button variant="ghost" onClick={() => setPendingDelete(null)}>
            Keep it
          </Button>
          {/* Keeping it is the first thing focus lands on: the destructive
              option should take a deliberate move to reach. */}
          <Button variant="danger" onClick={confirmDelete}>
            Delete conversation
          </Button>
        </div>
      </Modal>
    </div>
  )
}
