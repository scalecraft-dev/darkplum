import type { Conversation } from '../lib/conversations'
import styles from './ConversationList.module.css'

interface Props {
  conversations: Conversation[]
  activeId: string | null
  onSelect: (id: string) => void
  onNew: () => void
  onDelete: (id: string) => void
}

export function ConversationList({ conversations, activeId, onSelect, onNew, onDelete }: Props) {
  return (
    <div className={styles.panel}>
      <button type="button" className={styles.newBtn} onClick={onNew}>
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
          <line x1="8" y1="3" x2="8" y2="13" />
          <line x1="3" y1="8" x2="13" y2="8" />
        </svg>
        New chat
      </button>

      <ul className={styles.list} role="list">
        {conversations.map((c) => (
          <li key={c.id} className={`${styles.itemRow} ${c.id === activeId ? styles.active : ''}`}>
            <button
              type="button"
              className={styles.itemBtn}
              onClick={() => onSelect(c.id)}
              aria-current={c.id === activeId ? 'true' : undefined}
            >
              <span className={styles.title}>{c.title}</span>
            </button>
            <button
              type="button"
              className={styles.deleteBtn}
              onClick={() => onDelete(c.id)}
              aria-label={`Delete conversation: ${c.title}`}
            >
              <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
                <line x1="4" y1="4" x2="12" y2="12" />
                <line x1="12" y1="4" x2="4" y2="12" />
              </svg>
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}
