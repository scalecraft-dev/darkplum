import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { listLibrary } from '../../lib/api'
import type { LibraryItem, LibraryKind } from '../../types'
import { Badge, Button, Skeleton } from '../../components/ui'
import { KindGlyph } from '../../components/ui/KindGlyph'
import { KIND_NOUN, KIND_PLURAL, errorText, formatFull, formatRelative, formatSize, scopeText } from './libraryText'
import styles from '../LibraryPage.module.css'

type Scope = 'all' | 'mine' | 'shared'

const SCOPES: Array<{ value: Scope; label: string }> = [
  { value: 'all', label: 'All' },
  { value: 'mine', label: 'Mine' },
  { value: 'shared', label: 'Shared with me' },
]

interface Props {
  kind: LibraryKind
  /** Only the open tab reads; the others catch up when they are opened. */
  active: boolean
  refreshKey: number
  onOpen: (item: LibraryItem) => void
  onNew: () => void
  onImport: () => void
}

const EMPTY_COPY: Record<LibraryKind, { title: string; text: string; primary: 'import' | 'new'; primaryLabel: string; secondaryLabel: string }> = {
  skill: {
    title: 'Import instructions from a Markdown file',
    text: 'A skill is instructions the model follows for a chat. Import a Markdown file, such as the SKILL.md from a Claude skill or custom instructions you saved as text; only the instructions are kept, never scripts or other files. It then works on any model your organisation runs.',
    primary: 'import',
    primaryLabel: 'Import a file',
    secondaryLabel: 'Write a new skill',
  },
  tool: {
    title: 'Connect a tool server',
    text: 'A tool is an MCP server the model can call during a chat, such as an issue tracker or an internal search. Add its address, verify it, then attach it to a conversation. An administrator must allow the server\'s host first; nothing is called until then.',
    primary: 'new',
    primaryLabel: 'New tool',
    secondaryLabel: 'Import',
  },
  context: {
    title: 'Keep reference material in one place',
    text: 'Context is text a chat starts with: a style guide, a glossary, a product brief. Write it once and attach it wherever it helps.',
    primary: 'new',
    primaryLabel: 'New context',
    secondaryLabel: 'Import',
  },
}

/** One tab's list: the scope filter, the search field and the rows. */
export function LibraryList({ kind, active, refreshKey, onOpen, onNew, onImport }: Props) {
  const searchId = useId()
  const [scope, setScope] = useState<Scope>('all')
  const [query, setQuery] = useState('')
  const [q, setQ] = useState('')
  const [items, setItems] = useState<LibraryItem[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  // What the list on screen was read for, so a hidden tab that has fallen
  // behind re-reads when it is opened and not before.
  const loadedFor = useRef<string | null>(null)

  useEffect(() => {
    const id = setTimeout(() => setQ(query.trim()), 250)
    return () => clearTimeout(id)
  }, [query])

  const load = useCallback(async () => {
    const key = `${scope}|${q}|${refreshKey}`
    loadedFor.current = key
    setLoading(true)
    setError(null)
    try {
      const result = await listLibrary({ kind, scope, q: q || undefined })
      if (loadedFor.current === key) setItems(result)
    } catch (err) {
      if (loadedFor.current === key) setError(errorText(err, 'The library could not be loaded.'))
    } finally {
      if (loadedFor.current === key) setLoading(false)
    }
  }, [kind, scope, q, refreshKey])

  useEffect(() => {
    if (!active) return
    if (loadedFor.current === `${scope}|${q}|${refreshKey}`) return
    void load()
  }, [active, load, scope, q, refreshKey])

  const noun = KIND_NOUN[kind]
  const plural = KIND_PLURAL[kind]
  const filtered = scope !== 'all' || q !== ''

  let body: React.ReactNode
  if (error) {
    body = (
      <div className={styles.errorRow} role="alert">
        <span>The library could not be loaded: {error}</span>
        <Button variant="secondary" size="sm" onClick={() => void load()}>Try again</Button>
      </div>
    )
  } else if (items === null) {
    body = (
      <div className={styles.skeletons} aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} height="56px" borderRadius="var(--radius-md)" />
        ))}
      </div>
    )
  } else if (items.length === 0 && !filtered) {
    const copy = EMPTY_COPY[kind]
    const primary = copy.primary === 'import' ? onImport : onNew
    const secondary = copy.primary === 'import' ? onNew : onImport
    body = (
      <div className={styles.empty}>
        <span className={styles.emptyGlyph}><KindGlyph kind={kind} size={20} /></span>
        <h2 className={styles.emptyTitle}>{copy.title}</h2>
        <p className={styles.emptyText}>{copy.text}</p>
        <div className={styles.emptyActions}>
          <Button variant="primary" onClick={primary}>{copy.primaryLabel}</Button>
          <Button variant="secondary" onClick={secondary}>{copy.secondaryLabel}</Button>
        </div>
      </div>
    )
  } else if (items.length === 0) {
    body = (
      <div className={styles.emptySmall}>
        <p>
          {q !== ''
            ? `No ${plural} match “${q}”${scope === 'mine' ? ' among yours' : scope === 'shared' ? ' among those shared with you' : ''}.`
            : scope === 'mine'
              ? `You have no ${plural} of your own yet.`
              : `Nobody has shared ${kind === 'context' ? 'any context' : `a ${noun}`} with you yet.`}
        </p>
        {q !== '' ? (
          <Button variant="secondary" size="sm" onClick={() => { setQuery(''); setQ('') }}>Clear search</Button>
        ) : scope === 'mine' ? (
          <Button variant="secondary" size="sm" onClick={onNew}>New {noun}</Button>
        ) : null}
      </div>
    )
  } else {
    body = (
      <ul className={styles.rows} aria-label={`${plural[0].toUpperCase()}${plural.slice(1)}`} aria-busy={loading}>
        {items.map((item) => (
          <li key={item.id}>
            <button type="button" className={styles.row} aria-haspopup="dialog" onClick={() => onOpen(item)}>
              <span className={styles.rowGlyph}><KindGlyph kind={item.kind} /></span>
              <span className={styles.rowMain}>
                <span className={styles.rowName}>
                  <span className={styles.rowNameText}>{item.name}</span>
                  {item.kind === 'tool' && item.tool && item.tool.catalog_at === null && (
                    <Badge variant="neutral" dashed>Not verified</Badge>
                  )}
                </span>
                <span className={styles.rowDesc}>{item.description || 'No description'}</span>
              </span>
              <span className={styles.rowMeta}>
                <span className={item.visibility === 'org' ? styles.scopeShared : undefined}>{scopeText(item)}</span>
                <time dateTime={item.updated_at} title={`Edited ${formatFull(item.updated_at)}`}>
                  {formatRelative(item.updated_at)}
                </time>
                <span className={styles.rowSize}>{formatSize(item.size_bytes)}</span>
              </span>
            </button>
          </li>
        ))}
      </ul>
    )
  }

  return (
    <div className={styles.list}>
      <div className={styles.toolbar}>
        <div className={styles.segmented} role="group" aria-label={`Which ${plural} to show`}>
          {SCOPES.map((s) => (
            <button
              key={s.value}
              type="button"
              className={`${styles.segment} ${scope === s.value ? styles.segmentActive : ''}`}
              aria-pressed={scope === s.value}
              onClick={() => setScope(s.value)}
            >
              {s.label}
            </button>
          ))}
        </div>
        <div className={styles.searchWrap}>
          <svg className={styles.searchIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <label htmlFor={searchId} className={styles.srOnly}>Search {plural} by name or description</label>
          <input
            id={searchId}
            className={styles.searchInput}
            type="search"
            placeholder="Search by name or description"
            value={query}
            maxLength={100}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
      </div>
      {body}
    </div>
  )
}
