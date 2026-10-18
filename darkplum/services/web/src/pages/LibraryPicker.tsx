import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import type { LibraryItem } from '../types'
import { KindGlyph } from '../components/ui/KindGlyph'
import { KIND_LABEL } from './library/libraryText'
import styles from './LibraryPicker.module.css'


interface Props {
  items: LibraryItem[]
  loading: boolean
  error: string | null
  attachedIds: string[]
  onToggle: (item: LibraryItem) => void
  onClose: () => void
  onRefresh: () => void
  /** Fewer than this many can still be attached. */
  remaining: number
}

/**
 * The composer's "+" menu: everything the person can attach, searchable,
 * grouped into theirs and the organisation's. A tool that has never been
 * verified is listed but cannot be attached: the server's tools are unknown
 * until then, and the chat would refuse the turn.
 */
export function LibraryPicker({ items, loading, error, attachedIds, onToggle, onClose, onRefresh, remaining }: Props) {
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const listId = useId()

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const attached = useMemo(() => new Set(attachedIds), [attachedIds])
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    const match = (it: LibraryItem) =>
      !q || it.name.toLowerCase().includes(q) || it.description.toLowerCase().includes(q)
    const mine = items.filter((it) => it.mine && match(it))
    const shared = items.filter((it) => !it.mine && match(it))
    return { mine, shared, flat: [...mine, ...shared] }
  }, [items, query])

  useEffect(() => {
    setCursor(0)
  }, [query])

  useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(`[data-index="${cursor}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  }, [cursor])

  // A tool server that has never been verified has no known tools: the chat
  // would refuse the turn. A verified server with no tools is a different
  // state and is attachable (it just offers nothing).
  const unattachable = (it: LibraryItem) => it.kind === 'tool' && (!it.tool || it.tool.catalog_at === null)

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((c) => Math.min(c + 1, Math.max(visible.flat.length - 1, 0)))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((c) => Math.max(c - 1, 0))
    } else if (e.key === 'Enter') {
      const it = visible.flat[cursor]
      if (it && !unattachable(it) && (attached.has(it.id) || remaining > 0)) {
        e.preventDefault()
        onToggle(it)
      }
    }
  }

  const optionId = (index: number) => `${listId}-opt-${index}`

  const renderGroup = (label: string, group: LibraryItem[], offset: number) =>
    group.length > 0 && (
      <div className={styles.group} role="group" aria-label={label}>
        <div className={styles.groupLabel}>{label}</div>
        {group.map((it, i) => {
          const index = offset + i
          const isAttached = attached.has(it.id)
          const blocked = unattachable(it)
          const full = !isAttached && remaining <= 0
          // Why it cannot be attached, said on the row itself so keyboard and
          // touch readers see it, not only a mouse hover.
          const why = blocked ? 'Verify it in the library first' : full ? 'Limit reached' : ''
          return (
            <button
              key={it.id}
              id={optionId(index)}
              type="button"
              role="option"
              tabIndex={-1}
              aria-selected={isAttached}
              aria-disabled={blocked || full}
              data-index={index}
              className={`${styles.option} ${index === cursor ? styles.optionCursor : ''} ${isAttached ? styles.optionAttached : ''}`}
              onMouseEnter={() => setCursor(index)}
              onClick={() => {
                if (blocked || full) return
                onToggle(it)
              }}
            >
              <span className={styles.optionGlyph}>
                <KindGlyph kind={it.kind} size={14} />
              </span>
              <span className={styles.optionText}>
                <span className={styles.optionName}>{it.name}</span>
                <span className={styles.optionMeta}>
                  {KIND_LABEL[it.kind]}
                  {!it.mine ? ' · Shared' : ''}
                  {!it.mine && it.owner ? ` · by ${it.owner.name || it.owner.email}` : ''}
                  {why ? ` · ${why}` : ''}
                </span>
              </span>
              <span className={styles.optionCheck} aria-hidden="true">
                {isAttached && (
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                    <polyline points="20 6 9 17 4 12" />
                  </svg>
                )}
              </span>
            </button>
          )
        })}
      </div>
    )

  return (
    <div className={styles.panel} role="dialog" aria-label="Attach from your library" onKeyDown={onKeyDown}>
      <div className={styles.searchRow}>
        <input
          ref={inputRef}
          className={styles.search}
          placeholder="Search your library"
          aria-label="Search your library"
          role="combobox"
          aria-expanded="true"
          aria-controls={listId}
          aria-activedescendant={visible.flat.length > 0 ? optionId(cursor) : undefined}
          aria-autocomplete="list"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <span className={styles.remaining} aria-live="polite">
          {remaining > 0 ? `${remaining} more` : 'Limit reached'}
        </span>
      </div>
      <div className={styles.list} role="listbox" id={listId} aria-label="Library items" aria-multiselectable="true" ref={listRef}>
        {loading && items.length === 0 ? (
          <p className={styles.state}>Loading your library…</p>
        ) : error ? (
          <p className={styles.state}>
            {error}{' '}
            <button type="button" className={styles.inlineAction} onClick={onRefresh}>
              Try again
            </button>
          </p>
        ) : items.length === 0 ? (
          <p className={styles.state}>
            Nothing in your library yet.{' '}
            <Link to="/library?tab=skills">Import skills from Claude or ChatGPT</Link>, or write a skill,
            add a tool server or save some context on the Library page.
          </p>
        ) : visible.flat.length === 0 ? (
          <p className={styles.state}>No items match “{query}”.</p>
        ) : (
          <>
            {renderGroup('Mine', visible.mine, 0)}
            {renderGroup('Shared with me', visible.shared, visible.mine.length)}
          </>
        )}
      </div>
      <div className={styles.footer}>
        <Link to="/library" className={styles.footerLink}>
          Manage library
        </Link>
      </div>
    </div>
  )
}
