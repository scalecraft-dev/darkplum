import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import * as DropdownMenu from '@radix-ui/react-dropdown-menu'
import {
  ApiError,
  adminLibraryStatus,
  createLibraryFolder,
  deleteLibraryFolder,
  deleteLibraryItem,
  adminDeleteLibraryItem,
  exportLibrary,
  exportLibraryItem,
  listLibrary,
  listLibraryFolders,
  moveLibraryFolder,
  syncLibrary,
} from '../lib/api'
import { saveBlob } from '../lib/download'
import type { LibraryFolder, LibraryItem, LibraryKind, LibraryScope, LibraryStoreStatus, LibrarySyncResult } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { Button, Modal } from '../components/ui'
import { KindGlyph } from '../components/ui/KindGlyph'
import { FolderView } from './library/FolderView'
import { LibraryTree } from './library/LibraryTree'
import { MoveDialog } from './library/MoveDialog'
import { ItemDrawer, type DrawerTarget } from './library/ItemDrawer'
import { ImportModal } from './library/ImportModal'
import { ToolHostsCard } from './library/ToolHostsCard'
import { BucketGlyph, FolderGlyph, OrgGlyph, PrivateGlyph } from './library/LibraryGlyphs'
import { KIND_LABEL, KIND_NOUN, errorText, libraryErrorText } from './library/libraryText'
import {
  SCOPE_LABEL,
  buildTree,
  displayPath,
  joinPath,
  nameOf,
  parentOf,
  pathOf,
  scopeOf,
  segmentsOf,
  type TreeNode,
} from './library/libraryPath'
import styles from './LibraryPage.module.css'

type KindFilter = LibraryKind | 'all'

const KINDS: KindFilter[] = ['all', 'skill', 'context', 'tool']

function scopeFromParam(v: string | null): LibraryScope {
  return v === 'org' ? 'org' : 'private'
}

function kindFromParam(v: string | null): KindFilter {
  return v === 'skill' || v === 'context' || v === 'tool' ? v : 'all'
}

/** Old links: ?tab=tools meant the Tools tab; it now means the tool kind. */
function kindFromTab(v: string | null): KindFilter | null {
  return v === 'skills' ? 'skill' : v === 'tools' ? 'tool' : v === 'context' ? 'context' : null
}

function isValidPath(p: string): boolean {
  return p === '' || segmentsOf(p).every((s) => /^[a-z0-9][a-z0-9-]{0,63}$/.test(s))
}

/**
 * The library as a folder tree: two roots, Private and Organisation, that
 * mirror the bucket the items are kept in. The URL is the state
 * (?root=&path=&kind=&q=&item=), so a folder can be linked to and the back
 * button walks up. On a wide screen a tree pane shows the whole shape; on a
 * narrower one the breadcrumbs carry it.
 */
export function LibraryPage() {
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const { toast } = useToast()
  const [searchParams, setSearchParams] = useSearchParams()

  const legacyKind = kindFromTab(searchParams.get('tab'))
  const scope = scopeFromParam(searchParams.get('root'))
  const rawPath = searchParams.get('path') ?? ''
  const path = isValidPath(rawPath) ? rawPath : ''
  const kind = legacyKind ?? kindFromParam(searchParams.get('kind'))
  const query = searchParams.get('q') ?? ''
  const openItemId = searchParams.get('item')

  const setParams = useCallback(
    (patch: Record<string, string | null>, replace = false) => {
      setSearchParams(
        (prev) => {
          const params = new URLSearchParams(prev)
          params.delete('tab')
          for (const [k, v] of Object.entries(patch)) {
            if (v === null || v === '') params.delete(k)
            else params.set(k, v)
          }
          return params
        },
        { replace },
      )
    },
    [setSearchParams],
  )

  const folderHref = useCallback(
    (s: LibraryScope, p: string) => {
      const params = new URLSearchParams()
      if (s !== 'private') params.set('root', s)
      if (p) params.set('path', p)
      if (kind !== 'all') params.set('kind', kind)
      const qs = params.toString()
      return `/library${qs ? `?${qs}` : ''}`
    },
    [kind],
  )

  const navigate = useCallback(
    (s: LibraryScope, p: string) => setParams({ root: s === 'private' ? null : s, path: p, q: null, item: null }),
    [setParams],
  )

  // --- data: every item in both roots (one read each), the current folder's
  // children, and every folder path the page has seen, for the tree ---
  const [items, setItems] = useState<Record<LibraryScope, LibraryItem[] | null>>({ private: null, org: null })
  const [folders, setFolders] = useState<LibraryFolder[] | null>(null)
  /** Every folder under each root, so the tree is the bucket's shape, not
   *  only what has been browsed. */
  const [allFolders, setAllFolders] = useState<Record<LibraryScope, string[]>>({ private: [], org: [] })
  const [storeStatus, setStoreStatus] = useState<LibraryStoreStatus | null>(null)
  const [syncReport, setSyncReport] = useState<LibrarySyncResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  const refresh = useCallback(() => setRefreshKey((k) => k + 1), [])

  const loadItems = useCallback(async () => {
    const [priv, org, privTree, orgTree] = await Promise.all([
      listLibrary({ scope: 'private' }),
      listLibrary({ scope: 'org' }),
      listLibraryFolders('private', '', true).catch(() => ({ folders: [] as LibraryFolder[] })),
      listLibraryFolders('org', '', true).catch(() => ({ folders: [] as LibraryFolder[] })),
    ])
    setItems({ private: priv, org })
    setAllFolders({ private: privTree.folders.map((f) => f.path), org: orgTree.folders.map((f) => f.path) })
  }, [])

  const loadFolders = useCallback(async () => {
    const listing = await listLibraryFolders(scope, path)
    setFolders(listing.folders)
  }, [scope, path])

  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    adminLibraryStatus()
      .then((s) => {
        if (!cancelled) setStoreStatus(s)
      })
      .catch(() => {
        // An older api has no status route; the page says nothing.
      })
    return () => {
      cancelled = true
    }
  }, [isAdmin, refreshKey])

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    Promise.all([loadItems(), loadFolders()])
      .catch((err) => {
        if (cancelled) return
        // A link to a folder that is gone (or never was) says so by name.
        if (err instanceof ApiError && err.code() === 'folder_not_found') {
          setError(`there is no folder “${path}” under ${SCOPE_LABEL[scope]}. It may have been renamed or deleted.`)
          return
        }
        setError(errorText(err, 'The library could not be loaded.'))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [loadItems, loadFolders, refreshKey])

  const current = items[scope]
  const inFolder = useMemo(() => (current ?? []).filter((it) => pathOf(it) === path), [current, path])
  const results = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return null
    const all = [...(items.private ?? []), ...(items.org ?? [])]
    return all.filter(
      (it) =>
        it.name.toLowerCase().includes(q) ||
        it.description.toLowerCase().includes(q) ||
        pathOf(it).toLowerCase().includes(q),
    )
  }, [items, query])

  const trees = useMemo<Record<LibraryScope, TreeNode[]>>(() => {
    const paths = (s: LibraryScope) => {
      const set = new Set<string>(allFolders[s])
      for (const it of items[s] ?? []) if (pathOf(it)) set.add(pathOf(it))
      return set
    }
    return { private: buildTree(paths('private')), org: buildTree(paths('org')) }
  }, [items, allFolders])

  const otherRootCount = (items[scope === 'private' ? 'org' : 'private'] ?? []).length

  // --- drawer, dialogs ---
  const [drawer, setDrawer] = useState<DrawerTarget | null>(null)
  const [importOpen, setImportOpen] = useState(false)
  const [moving, setMoving] = useState<LibraryItem | null>(null)
  const [deletingFolder, setDeletingFolder] = useState<LibraryFolder | null>(null)
  const [deletingItem, setDeletingItem] = useState<LibraryItem | null>(null)
  const [newFolder, setNewFolder] = useState<{ active: boolean; error: string | null }>({ active: false, error: null })
  const [renaming, setRenaming] = useState<{ path: string; error: string | null } | null>(null)
  const [exporting, setExporting] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const [busy, setBusy] = useState(false)
  const hostsHeading = useRef<HTMLHeadingElement>(null)

  // ?item=<id> opens the drawer once the item is known.
  useEffect(() => {
    if (!openItemId || drawer) return
    const it = [...(items.private ?? []), ...(items.org ?? [])].find((x) => x.id === openItemId)
    if (it) setDrawer({ mode: 'open', item: it })
  }, [openItemId, items, drawer])

  const openItem = (item: LibraryItem) => {
    setDrawer({ mode: 'open', item })
    setParams({ item: item.id }, true)
  }
  const closeDrawer = () => {
    setDrawer(null)
    setParams({ item: null }, true)
  }

  const exportAll = async () => {
    setExporting(true)
    try {
      const { blob, filename } = await exportLibrary()
      saveBlob(blob, filename)
      toast(`Exported your library as ${filename}.`, 'success')
    } catch (err) {
      toast(errorText(err, 'The library could not be exported.'), 'error')
    } finally {
      setExporting(false)
    }
  }

  const exportItem = async (item: LibraryItem) => {
    try {
      const { blob, filename } = await exportLibraryItem(item.id)
      saveBlob(blob, filename)
    } catch (err) {
      toast(errorText(err, 'The item could not be exported.'), 'error')
    }
  }

  const sync = async () => {
    setSyncing(true)
    try {
      const r = await syncLibrary(scope)
      if (r.imported === 0 && r.updated === 0 && r.conflicts.length === 0 && r.errors.length === 0 && !r.truncated) {
        toast('The bucket and the library already agree.', 'success')
      } else {
        setSyncReport(r)
      }
      refresh()
    } catch (err) {
      toast(libraryErrorText(err, 'The bucket could not be read.'), 'error')
    } finally {
      setSyncing(false)
    }
  }

  const commitNewFolder = async (name: string) => {
    const full = joinPath(path, name)
    setBusy(true)
    try {
      await createLibraryFolder(scope, full)
      setNewFolder({ active: false, error: null })
      refresh()
    } catch (err) {
      setNewFolder({ active: true, error: libraryErrorText(err, 'The folder could not be created.') })
    } finally {
      setBusy(false)
    }
  }

  const commitRename = async (from: string, name: string) => {
    const to = joinPath(parentOf(from), name)
    if (to === from) {
      setRenaming(null)
      return
    }
    setBusy(true)
    try {
      const r = await moveLibraryFolder(scope, from, to)
      setRenaming(null)
      toast(`Renamed to ${name}${r.moved > 0 ? ` (${r.moved} ${r.moved === 1 ? 'item' : 'items'})` : ''}.`, 'success')
      refresh()
    } catch (err) {
      setRenaming({ path: from, error: libraryErrorText(err, 'The folder could not be renamed.') })
    } finally {
      setBusy(false)
    }
  }

  const confirmDeleteFolder = async () => {
    if (!deletingFolder) return
    setBusy(true)
    try {
      const r = await deleteLibraryFolder(scope, deletingFolder.path, true)
      toast(`Deleted ${deletingFolder.name}${r.items > 0 ? ` and ${r.items} ${r.items === 1 ? 'item' : 'items'}` : ''}.`, 'success')
      setDeletingFolder(null)
      refresh()
    } catch (err) {
      toast(libraryErrorText(err, 'The folder could not be deleted.'), 'error')
    } finally {
      setBusy(false)
    }
  }

  const confirmDeleteItem = async () => {
    if (!deletingItem) return
    setBusy(true)
    try {
      if (deletingItem.mine) await deleteLibraryItem(deletingItem.id)
      else await adminDeleteLibraryItem(deletingItem.id)
      toast(`Deleted ${deletingItem.name}.`, 'success')
      setDeletingItem(null)
      refresh()
    } catch (err) {
      toast(libraryErrorText(err, 'The item could not be deleted.'), 'error')
    } finally {
      setBusy(false)
    }
  }

  /** From a refused verify: the drawer has closed; once its focus has gone
   *  back to the row, move it on to the allowed-servers card. */
  const showHosts = useCallback(() => {
    setParams({ kind: 'tool' })
    window.setTimeout(() => {
      const el = hostsHeading.current
      if (!el) return
      const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      el.scrollIntoView({ block: 'start', behavior: reduce ? 'auto' : 'smooth' })
      el.focus({ preventScroll: true })
    }, 250)
  }, [setParams])

  const crumbs = segmentsOf(path)
  const searching = query.trim() !== ''

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <div className={styles.headerText}>
          <h1 className={styles.display}>Library</h1>
          <p className={styles.lede}>
            Skills, tools and context, kept as files in folders. Private is yours alone; Organisation is shared with
            everyone.
          </p>
        </div>
        <div className={styles.headerActions}>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void sync()}
            disabled={syncing || (scope === 'org' && !isAdmin)}
            title={
              scope === 'org' && !isAdmin
                ? 'An administrator syncs the Organisation folder.'
                : 'Read files added or changed in the bucket into this root'
            }
          >
            <BucketGlyph size={14} />
            {syncing ? 'Syncing…' : 'Sync from bucket'}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void exportAll()} disabled={exporting} title="Everything you can see, as a zip of .md and .json files in their folders">
            {exporting ? 'Exporting…' : 'Export all'}
          </Button>
          <Button variant="secondary" size="sm" onClick={() => setImportOpen(true)}>
            Import
          </Button>
          <DropdownMenu.Root modal={false}>
            <DropdownMenu.Trigger asChild>
              <Button variant="primary" size="sm">
                New
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <polyline points="6 9 12 15 18 9" />
                </svg>
              </Button>
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content className={styles.menu} align="end" sideOffset={4}>
                {(['skill', 'context', 'tool'] as LibraryKind[]).map((k) => (
                  <DropdownMenu.Item key={k} className={styles.menuItem} onSelect={() => setDrawer({ mode: 'new', kind: k, scope, path })}>
                    <KindGlyph kind={k} size={14} />
                    New {KIND_NOUN[k]}
                  </DropdownMenu.Item>
                ))}
                <DropdownMenu.Separator className={styles.menuSep} />
                <DropdownMenu.Item className={styles.menuItem} onSelect={() => setNewFolder({ active: true, error: null })}>
                  <FolderGlyph size={14} />
                  New folder
                </DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
          </DropdownMenu.Root>
        </div>
      </header>

      {storeStatus && (!storeStatus.reachable || storeStatus.failing > 0 || (storeStatus.changed_in_bucket ?? 0) > 0) && (
        <div className={`${styles.storeBanner} ${!storeStatus.reachable ? styles.storeBannerDanger : ''}`} role="status">
          <BucketGlyph size={14} />
          <span>
            {!storeStatus.reachable
              ? `The bucket ${storeStatus.bucket || ''} cannot be reached. Items still work here; nothing is being written to the bucket until it is back.`
              : storeStatus.failing > 0
                ? `${storeStatus.failing} ${storeStatus.failing === 1 ? 'item is' : 'items are'} not reaching the bucket. The api's log says why, by item id.`
                : `${storeStatus.changed_in_bucket} ${storeStatus.changed_in_bucket === 1 ? 'file was' : 'files were'} changed in the bucket outside the app. Sync from bucket adopts them.`}
            {storeStatus.queue_depth > 0 ? ` ${storeStatus.queue_depth} waiting.` : ''}
          </span>
        </div>
      )}

      <div className={styles.location}>
        <div className={styles.segmented} role="group" aria-label="Root">
          {(['private', 'org'] as LibraryScope[]).map((s) => (
            <button
              key={s}
              type="button"
              className={`${styles.segment} ${scope === s && !searching ? styles.segmentActive : ''}`}
              aria-pressed={scope === s}
              onClick={() => navigate(s, '')}
            >
              {s === 'private' ? <PrivateGlyph size={14} /> : <OrgGlyph size={14} />}
              {SCOPE_LABEL[s]}
            </button>
          ))}
        </div>
        {searching ? (
          <div className={styles.crumbs}>
            <span className={styles.crumbCurrent}>
              Results for “{query.trim()}” · {(results ?? []).filter((it) => kind === 'all' || it.kind === kind).length}
            </span>
            <button type="button" className={styles.crumbClear} onClick={() => setParams({ q: null })}>
              Clear
            </button>
          </div>
        ) : (
          <nav className={styles.crumbs} aria-label="Folder path">
            <ol>
              {crumbs.map((seg, i) => {
                const p = crumbs.slice(0, i + 1).join('/')
                const last = i === crumbs.length - 1
                return (
                  <li key={p} className={styles.crumbPair}>
                    <span className={styles.crumbSep} aria-hidden="true">
                      /
                    </span>
                    {last ? (
                      <span className={`${styles.crumb} ${styles.crumbCurrent}`} aria-current="page">
                        {seg}
                      </span>
                    ) : (
                      <Link className={styles.crumb} to={folderHref(scope, p)}>
                        {seg}
                      </Link>
                    )}
                  </li>
                )
              })}
            </ol>
            {path !== '' && (
              <Link className={styles.crumbUp} to={folderHref(scope, parentOf(path))} aria-label={`Back to ${nameOf(parentOf(path)) || SCOPE_LABEL[scope]}`}>
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <polyline points="15 18 9 12 15 6" />
                </svg>
              </Link>
            )}
          </nav>
        )}
      </div>

      <div className={styles.toolbar}>
        <div className={styles.kinds} role="group" aria-label="Kind">
          {KINDS.map((k) => (
            <button
              key={k}
              type="button"
              className={`${styles.chip} ${kind === k ? styles.chipActive : ''}`}
              aria-pressed={kind === k}
              onClick={() => setParams({ kind: k === 'all' ? null : k })}
            >
              {k === 'all' ? 'All kinds' : KIND_LABEL[k]}
            </button>
          ))}
        </div>
        <div className={styles.searchWrap}>
          <svg className={styles.searchIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            className={styles.searchInput}
            type="search"
            placeholder="Search all folders"
            aria-label="Search all folders"
            value={query}
            maxLength={100}
            onChange={(e) => setParams({ q: e.target.value }, true)}
          />
        </div>
        <Button variant="secondary" size="sm" onClick={() => setNewFolder({ active: true, error: null })} disabled={searching || newFolder.active || (scope === 'org' && !isAdmin)}>
          New folder
        </Button>
      </div>

      <div className={styles.body}>
        <LibraryTree trees={trees} current={{ scope, path }} onNavigate={navigate} />
        <div className={styles.main}>
          {kind === 'tool' && isAdmin && <ToolHostsCard ref={hostsHeading} refreshKey={refreshKey} />}
          <FolderView
            scope={scope}
            path={path}
            kind={kind}
            query={query}
            folders={folders}
            items={inFolder}
            results={results}
            loading={loading || busy}
            error={error}
            isAdmin={isAdmin}
            otherRootCount={otherRootCount}
            newFolder={newFolder}
            renaming={renaming}
            folderHref={folderHref}
            onRetry={refresh}
            onOpen={openItem}
            onNew={(k) => setDrawer({ mode: 'new', kind: k, scope, path })}
            onImport={() => setImportOpen(true)}
            onClearSearch={() => setParams({ q: null })}
            onClearKind={() => setParams({ kind: null })}
            onStartNewFolder={() => setNewFolder({ active: true, error: null })}
            onCommitNewFolder={(name) => void commitNewFolder(name)}
            onCancelNewFolder={() => setNewFolder({ active: false, error: null })}
            onStartRename={(p) => setRenaming({ path: p, error: null })}
            onCommitRename={(from, name) => void commitRename(from, name)}
            onCancelRename={() => setRenaming(null)}
            onMoveItem={setMoving}
            onDeleteFolder={setDeletingFolder}
            onDeleteItem={setDeletingItem}
            onExportItem={(it) => void exportItem(it)}
            onBrowseOtherRoot={() => navigate(scope === 'private' ? 'org' : 'private', '')}
          />
        </div>
      </div>

      <ItemDrawer
        target={drawer}
        isAdmin={isAdmin}
        onClose={closeDrawer}
        onChanged={refresh}
        onOpen={(item) => setDrawer({ mode: 'open', item })}
        onShowHosts={showHosts}
        onMove={(item) => setMoving(item)}
      />

      <ImportModal
        open={importOpen}
        kind={kind === 'all' ? 'skill' : kind}
        scope={scope}
        path={path}
        onClose={() => setImportOpen(false)}
        onImported={(n) => {
          if (n > 0) refresh()
        }}
      />

      <MoveDialog
        item={moving}
        canCrossRoots={moving?.mine ?? false}
        onClose={() => setMoving(null)}
        onMoved={(next, from) => {
          setMoving(null)
          if (!next) {
            toast('Unshared. It is private to its owner again.', 'success')
            if (drawer?.mode === 'open' && drawer.item.id === moving?.id) closeDrawer()
            refresh()
            return
          }
          const to = scopeOf(next)
          toast(
            from.scope !== to
              ? to === 'org'
                ? `Shared ${next.name} with your organisation.`
                : `Unshared ${next.name}.`
              : `Moved ${next.name}.`,
            'success',
          )
          if (drawer?.mode === 'open' && drawer.item.id === next.id) setDrawer({ mode: 'open', item: next })
          refresh()
        }}
      />

      <Modal open={syncReport !== null} onClose={() => setSyncReport(null)} title="Synced from the bucket">
        {syncReport && (
          <div className={styles.syncReport}>
            <p>
              {[
                `${syncReport.imported} imported`,
                `${syncReport.updated} updated`,
                `${syncReport.skipped} unchanged`,
              ].join(' · ')}
              {syncReport.scanned !== undefined ? ` · ${syncReport.scanned} files read` : ''}
            </p>
            {syncReport.truncated && (
              <p className={styles.dialogHint}>The bucket has more files than one pass reads. Run Sync again for the rest.</p>
            )}
            {syncReport.conflicts.length > 0 && (
              <div>
                <p>
                  Left as they are, because they changed here and in the bucket. The library's version wins; the bucket's stays in
                  its version history:
                </p>
                <ul>
                  {syncReport.conflicts.map((id) => {
                    const it = [...(items.private ?? []), ...(items.org ?? [])].find((x) => x.id === id)
                    return <li key={id}>{it ? `${it.name} (${displayPath(pathOf(it), scopeOf(it))})` : id}</li>
                  })}
                </ul>
              </div>
            )}
            {syncReport.errors.length > 0 && (
              <div>
                <p>Could not be read. Files are named {'<name>'}.skill.md, .context.md or .tool.json, in lowercase hyphenated folders:</p>
                <ul>
                  {syncReport.errors.map((e, i) => (
                    <li key={i}>
                      <code>{e.file_name}</code> — {e.reason}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            <div className={styles.dialogActions}>
              <Button variant="primary" onClick={() => setSyncReport(null)}>
                Done
              </Button>
            </div>
          </div>
        )}
      </Modal>

      <Modal
        open={deletingFolder !== null}
        onClose={() => setDeletingFolder(null)}
        title={deletingFolder ? `Delete “${deletingFolder.name}” and everything in it?` : 'Delete folder'}
        dismissible={!busy}
      >
        {deletingFolder && (
          <div className={styles.dialogBody}>
            <p>
              {deletingFolder.folders === 0 && deletingFolder.items === 0
                ? 'The folder is empty.'
                : `${deletingFolder.folders} ${deletingFolder.folders === 1 ? 'folder' : 'folders'} and ${deletingFolder.items} ${deletingFolder.items === 1 ? 'item' : 'items'}. Chats that attach them lose them from the next message.`}
            </p>
            <p className={styles.dialogHint}>An administrator can recover deleted files from the bucket's version history.</p>
            <div className={styles.dialogActions}>
              <Button variant="secondary" onClick={() => setDeletingFolder(null)} disabled={busy}>
                Cancel
              </Button>
              <Button variant="danger" onClick={() => void confirmDeleteFolder()} disabled={busy}>
                {busy ? 'Deleting…' : deletingFolder.items > 0 ? `Delete ${deletingFolder.items + deletingFolder.folders}` : 'Delete folder'}
              </Button>
            </div>
          </div>
        )}
      </Modal>

      <Modal open={deletingItem !== null} onClose={() => setDeletingItem(null)} title={deletingItem ? `Delete “${deletingItem.name}”?` : 'Delete'} dismissible={!busy}>
        {deletingItem && (
          <div className={styles.dialogBody}>
            <p>
              {scopeOf(deletingItem) === 'org'
                ? 'Everyone who attached it loses it from their next message.'
                : 'Chats that attach it lose it from the next message.'}
            </p>
            <div className={styles.dialogActions}>
              <Button variant="secondary" onClick={() => setDeletingItem(null)} disabled={busy}>
                Cancel
              </Button>
              <Button variant="danger" onClick={() => void confirmDeleteItem()} disabled={busy}>
                {busy ? 'Deleting…' : 'Delete'}
              </Button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  )
}
