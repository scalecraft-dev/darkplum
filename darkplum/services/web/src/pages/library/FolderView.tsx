import { useState } from 'react'
import { Link } from 'react-router-dom'
import * as DropdownMenu from '@radix-ui/react-dropdown-menu'
import type { LibraryFolder, LibraryItem, LibraryKind, LibraryScope } from '../../types'
import { Badge, Button, Skeleton } from '../../components/ui'
import { KindGlyph } from '../../components/ui/KindGlyph'
import { FolderGlyph } from './LibraryGlyphs'
import { KIND_LABEL, formatFull, formatRelative, formatSize, ownerName } from './libraryText'
import { displayPath, fileNameOf, folderNameProblem, nameOf, pathOf, scopeOf, segmentsOf, toSegment } from './libraryPath'
import styles from '../LibraryPage.module.css'

export interface FolderViewProps {
  scope: LibraryScope
  path: string
  kind: LibraryKind | 'all'
  query: string
  folders: LibraryFolder[] | null
  items: LibraryItem[] | null
  /** Items matching the search across both roots, or null when not searching. */
  results: LibraryItem[] | null
  loading: boolean
  error: string | null
  isAdmin: boolean
  /** The other root holds something while this one is empty. */
  otherRootCount: number
  newFolder: { active: boolean; error: string | null }
  renaming: { path: string; error: string | null } | null
  folderHref: (scope: LibraryScope, path: string) => string
  onRetry: () => void
  onOpen: (item: LibraryItem) => void
  onNew: (kind: LibraryKind) => void
  onImport: () => void
  onClearSearch: () => void
  onClearKind: () => void
  onStartNewFolder: () => void
  onCommitNewFolder: (name: string) => void
  onCancelNewFolder: () => void
  onStartRename: (path: string) => void
  onCommitRename: (path: string, name: string) => void
  onCancelRename: () => void
  onMoveItem: (item: LibraryItem) => void
  onDeleteFolder: (folder: LibraryFolder) => void
  onDeleteItem: (item: LibraryItem) => void
  onExportItem: (item: LibraryItem) => void
  onBrowseOtherRoot: () => void
}

const MenuGlyph = () => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <circle cx="12" cy="12" r="1" />
    <circle cx="19" cy="12" r="1" />
    <circle cx="5" cy="12" r="1" />
  </svg>
)

/** One row's actions. A real menu (keyboard, focus return, typeahead), not a
 *  div that only answers clicks. Always visible: touch has no hover. */
function RowMenu({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <DropdownMenu.Root modal={false}>
      <DropdownMenu.Trigger asChild>
        <button type="button" className={styles.rowMenu} aria-label={label}>
          <MenuGlyph />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content className={styles.menu} align="end" sideOffset={4} collisionPadding={8}>
          {children}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}

function MenuItem({
  onSelect,
  danger,
  disabled,
  reason,
  children,
}: {
  onSelect: () => void
  danger?: boolean
  disabled?: boolean
  reason?: string
  children: React.ReactNode
}) {
  return (
    <DropdownMenu.Item className={`${styles.menuItem} ${danger ? styles.menuItemDanger : ''}`} disabled={disabled} onSelect={onSelect}>
      <span>{children}</span>
      {disabled && reason && <span className={styles.menuReason}>{reason}</span>}
    </DropdownMenu.Item>
  )
}

function InlineNameRow({
  initial,
  depthOfParent,
  placeholder,
  error,
  onCommit,
  onCancel,
}: {
  initial: string
  depthOfParent: number
  placeholder: string
  error: string | null
  onCommit: (name: string) => void
  onCancel: () => void
}) {
  const [value, setValue] = useState(initial)
  const problem = value.trim() === '' ? null : folderNameProblem(toSegment(value), depthOfParent)
  const shown = error ?? problem
  const commit = () => {
    if (value.trim() === '') {
      onCancel()
      return
    }
    if (problem) return
    onCommit(toSegment(value))
  }
  return (
    <li className={styles.rowInline}>
      <span className={styles.rowGlyph}>
        <FolderGlyph />
      </span>
      <form
        className={styles.inlineForm}
        onSubmit={(e) => {
          e.preventDefault()
          commit()
        }}
      >
        <input
          className={styles.inlineInput}
          value={value}
          autoFocus
          placeholder={placeholder}
          aria-label="Folder name"
          aria-invalid={shown ? true : undefined}
          onChange={(e) => setValue(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.preventDefault()
              onCancel()
            }
          }}
        />
        <span className={shown ? styles.inlineError : styles.inlineHint}>
          {shown ?? (value.trim() ? `Saved as ${toSegment(value) || '…'}` : 'Letters, numbers and hyphens. Up to 64.')}
        </span>
      </form>
    </li>
  )
}

export function FolderView(p: FolderViewProps) {
  const searching = p.query.trim() !== ''
  const depth = segmentsOf(p.path).length

  if (p.error) {
    return (
      <div className={styles.errorRow} role="alert">
        <span>This folder could not be loaded: {p.error}</span>
        <Button variant="secondary" size="sm" onClick={p.onRetry}>
          Try again
        </Button>
      </div>
    )
  }

  if ((p.items === null || (!searching && p.folders === null)) && !p.error) {
    return (
      <div className={styles.skeletons} aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} height="56px" borderRadius="var(--radius-md)" />
        ))}
      </div>
    )
  }

  // --- search results across both roots ---
  if (searching) {
    const results = (p.results ?? []).filter((it) => p.kind === 'all' || it.kind === p.kind)
    if (results.length === 0) {
      return (
        <div className={styles.emptySmall}>
          <p>Nothing matches “{p.query.trim()}” in any folder.</p>
          <Button variant="secondary" size="sm" onClick={p.onClearSearch}>
            Clear search
          </Button>
        </div>
      )
    }
    const shown = results.slice(0, 50)
    return (
      <>
        <ul className={styles.rows} aria-label="Search results">
          {shown.map((item) => (
            <ItemRow key={item.id} item={item} showPath p={p} />
          ))}
        </ul>
        {results.length > shown.length && (
          <p className={styles.resultsNote}>
            Showing {shown.length} of {results.length}. Add words to narrow it.
          </p>
        )}
      </>
    )
  }

  const folders = p.folders ?? []
  const items = (p.items ?? []).filter((it) => p.kind === 'all' || it.kind === p.kind)
  const empty = folders.length === 0 && items.length === 0 && !p.newFolder.active

  if (empty && p.path === '' && p.kind === 'all') {
    if (p.scope === 'private' && p.otherRootCount > 0) {
      return (
        <div className={styles.empty}>
          <span className={styles.emptyGlyph}>
            <FolderGlyph size={20} />
          </span>
          <h2 className={styles.emptyTitle}>Nothing private yet</h2>
          <p className={styles.emptyText}>
            Your organisation has shared {p.otherRootCount} {p.otherRootCount === 1 ? 'item' : 'items'}. Browse them, or start your own.
          </p>
          <div className={styles.emptyActions}>
            <Button variant="primary" onClick={p.onBrowseOtherRoot}>
              Browse Organisation
            </Button>
            <Button variant="secondary" onClick={() => p.onNew('skill')}>
              New skill
            </Button>
            <Button variant="secondary" onClick={p.onImport}>
              Import
            </Button>
          </div>
        </div>
      )
    }
    if (p.scope === 'org') {
      return (
        <div className={styles.empty}>
          <span className={styles.emptyGlyph}>
            <FolderGlyph size={20} />
          </span>
          <h2 className={styles.emptyTitle}>Nothing shared yet</h2>
          <p className={styles.emptyText}>
            Items moved here are readable by everyone at your organisation, and only their owner edits them. Start in
            Private, then move what is worth sharing.
          </p>
          <div className={styles.emptyActions}>
            <Button variant="primary" onClick={p.onBrowseOtherRoot}>
              Go to Private
            </Button>
          </div>
        </div>
      )
    }
    return (
      <div className={styles.empty}>
        <span className={styles.emptyGlyph}>
          <KindGlyph kind="skill" size={20} />
        </span>
        <h2 className={styles.emptyTitle}>Import instructions from a Markdown file</h2>
        <p className={styles.emptyText}>
          A skill is instructions the model follows for a chat. Import a Markdown file, such as the SKILL.md from a
          Claude skill or custom instructions you saved as text; only the instructions are kept, never scripts or other
          files. It then works on any model your organisation runs. Context is reference text a chat starts with; a
          tool is an MCP server the model may call.
        </p>
        <div className={styles.emptyActions}>
          <Button variant="primary" onClick={p.onImport}>
            Import a file
          </Button>
          <Button variant="secondary" onClick={() => p.onNew('skill')}>
            Write a new skill
          </Button>
          <Button variant="secondary" onClick={p.onStartNewFolder}>
            New folder
          </Button>
        </div>
      </div>
    )
  }

  return (
    <>
      <ul className={styles.rows} aria-label={`Contents of ${displayPath(p.path, p.scope)}`} aria-busy={p.loading}>
        {p.newFolder.active && (
          <InlineNameRow
            initial=""
            depthOfParent={depth}
            placeholder="folder-name"
            error={p.newFolder.error}
            onCommit={p.onCommitNewFolder}
            onCancel={p.onCancelNewFolder}
          />
        )}
        {folders.map((f) =>
          p.renaming?.path === f.path ? (
            <InlineNameRow
              key={f.path}
              initial={f.name}
              depthOfParent={depth}
              placeholder={f.name}
              error={p.renaming.error}
              onCommit={(name) => p.onCommitRename(f.path, name)}
              onCancel={p.onCancelRename}
            />
          ) : (
            <li key={f.path} className={styles.rowLi}>
              <Link className={styles.row} to={p.folderHref(p.scope, f.path)}>
                <span className={styles.rowGlyph}>
                  <FolderGlyph />
                </span>
                <span className={styles.rowMain}>
                  <span className={styles.rowName}>
                    <span className={styles.rowNameText}>{f.name}</span>
                  </span>
                  <span className={styles.rowDesc}>
                    {f.folders === 0 && f.items === 0
                      ? 'Empty'
                      : [
                          f.folders > 0 ? `${f.folders} ${f.folders === 1 ? 'folder' : 'folders'}` : '',
                          f.items > 0 ? `${f.items} ${f.items === 1 ? 'item' : 'items'}` : '',
                        ]
                          .filter(Boolean)
                          .join(' · ')}
                  </span>
                </span>
                <span className={styles.rowMeta}>
                  {f.updated_at && (
                    <time dateTime={f.updated_at} title={`Edited ${formatFull(f.updated_at)}`}>
                      {formatRelative(f.updated_at)}
                    </time>
                  )}
                </span>
              </Link>
              <RowMenu label={`Actions for ${f.name}`}>
                <MenuItem onSelect={() => p.onStartRename(f.path)} disabled={p.scope === 'org' && !p.isAdmin} reason="Holds items other people shared. Ask an administrator.">
                  Rename
                </MenuItem>
                <MenuItem onSelect={() => p.onDeleteFolder(f)} danger disabled={p.scope === 'org' && !p.isAdmin} reason="Holds items other people shared. Ask an administrator.">
                  Delete folder…
                </MenuItem>
              </RowMenu>
            </li>
          ),
        )}
        {items.map((item) => (
          <ItemRow key={item.id} item={item} p={p} />
        ))}
      </ul>
      {folders.length === 0 && items.length === 0 && !p.newFolder.active && (
        <div className={styles.emptySmall}>
          <p>
            {p.kind === 'tool'
              ? `No tools in ${nameOf(p.path) || displayPath('', p.scope)}. A tool is an MCP server the model may call: add its address and it is checked when you save.${p.isAdmin ? '' : ' A server nobody has used yet needs an administrator to allow it.'}`
              : p.kind !== 'all'
                ? `No ${KIND_LABEL[p.kind].toLowerCase()} items in ${nameOf(p.path) || displayPath('', p.scope)}.`
                : `Nothing in ${nameOf(p.path)} yet.`}
          </p>
          {p.kind !== 'all' ? (
            <Button variant="secondary" size="sm" onClick={p.onClearKind}>
              Show all kinds
            </Button>
          ) : (
            <>
              <Button variant="secondary" size="sm" onClick={() => p.onNew('skill')}>
                New skill
              </Button>
              <Button variant="secondary" size="sm" onClick={() => p.onNew('context')}>
                New context
              </Button>
              <Button variant="secondary" size="sm" onClick={p.onImport}>
                Import here
              </Button>
            </>
          )}
        </div>
      )}
    </>
  )
}

function ItemRow({ item, showPath = false, p }: { item: LibraryItem; showPath?: boolean; p: FolderViewProps }) {
  const scope = scopeOf(item)
  const path = pathOf(item)
  return (
    <li className={styles.rowLi}>
      <button type="button" className={styles.row} aria-haspopup="dialog" onClick={() => p.onOpen(item)}>
        <span className={styles.rowGlyph}>
          <KindGlyph kind={item.kind} />
        </span>
        <span className={styles.rowMain}>
          <span className={styles.rowName}>
            <span className={styles.rowNameText}>{item.name}</span>
            {item.kind === 'tool' && item.tool && item.tool.catalog_at === null && (
              <Badge variant="neutral" dashed>
                Not verified
              </Badge>
            )}
            {item.bucket_changed ? (
              <Badge variant="warning" bordered title="The file in the bucket was changed outside the app. Sync from bucket adopts it.">
                Changed in the bucket
              </Badge>
            ) : item.synced === false ? (
              <span className={styles.syncDot} title="Not in the bucket yet; it will be shortly." aria-label="Syncing to the bucket" />
            ) : null}
          </span>
          {showPath ? (
            <span className={styles.rowPath} title={displayPath(path, scope)}>
              <span className={styles.rowPathText}>{displayPath(path, scope)}</span>
            </span>
          ) : (
            <span className={styles.rowDesc} title={item.description || undefined}>
              {item.description || 'No description'}
            </span>
          )}
        </span>
        <span className={styles.rowMeta}>
          <code className={styles.rowFile} title="The file in the bucket">{fileNameOf(item)}</code>
          <span className={styles.rowKind}>{KIND_LABEL[item.kind]}</span>
          {scope === 'org' && <span>{item.mine ? 'by you' : `by ${ownerName(item)}`}</span>}
          <time dateTime={item.updated_at} title={`Edited ${formatFull(item.updated_at)}`}>
            {formatRelative(item.updated_at)}
          </time>
          <span className={styles.rowSize}>{formatSize(item.size_bytes)}</span>
        </span>
      </button>
      <RowMenu label={`Actions for ${item.name}`}>
        <MenuItem onSelect={() => p.onOpen(item)}>Open</MenuItem>
        {showPath && (
          <DropdownMenu.Item className={styles.menuItem} asChild>
            <Link to={p.folderHref(scope, path)}>Show in folder</Link>
          </DropdownMenu.Item>
        )}
        <MenuItem onSelect={() => p.onMoveItem(item)} disabled={!item.mine && !(p.isAdmin && scope === 'org')} reason="Only the owner can move it.">
          Move…
        </MenuItem>
        <MenuItem onSelect={() => p.onExportItem(item)}>Export {item.kind === 'tool' ? '.json' : '.md'}</MenuItem>
        <MenuItem onSelect={() => p.onDeleteItem(item)} danger disabled={!item.mine && !(p.isAdmin && scope === 'org')} reason="Only the owner can delete it.">
          Delete…
        </MenuItem>
      </RowMenu>
    </li>
  )
}
