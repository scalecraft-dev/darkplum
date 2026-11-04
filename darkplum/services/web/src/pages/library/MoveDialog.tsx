import { useEffect, useState } from 'react'
import { createLibraryFolder, listLibraryFolders, moveLibraryItem } from '../../lib/api'
import type { LibraryFolder, LibraryItem, LibraryScope } from '../../types'
import { Button, Modal } from '../../components/ui'
import { FolderGlyph, OrgGlyph, PrivateGlyph } from './LibraryGlyphs'
import { SCOPE_LABEL, folderNameProblem, joinPath, nameOf, parentOf, pathOf, scopeOf, segmentsOf, toSegment } from './libraryPath'
import { ApiError } from '../../lib/api'
import { libraryErrorText } from './libraryText'
import styles from './LibraryDialogs.module.css'

interface Props {
  item: LibraryItem | null
  /** Only an owner may cross roots; an administrator moves another person's
   *  shared item within Organisation. */
  canCrossRoots: boolean
  onClose: () => void
  /** `item` is null when the move took it out of the caller's sight (an
   *  administrator unsharing someone else's item). */
  onMoved: (item: LibraryItem | null, from: { scope: LibraryScope; path: string }) => void
}

/**
 * Where an item goes: a root, then a folder, chosen by browsing. No drag and
 * drop: a drop from Private into Organisation would share something with the
 * whole company by accident, and a phone has no drag anyway.
 */
export function MoveDialog({ item, canCrossRoots, onClose, onMoved }: Props) {
  const [scope, setScope] = useState<LibraryScope>('private')
  const [path, setPath] = useState('')
  const [folders, setFolders] = useState<LibraryFolder[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [newName, setNewName] = useState<string | null>(null)

  useEffect(() => {
    if (!item) return
    setScope(scopeOf(item))
    setPath(pathOf(item))
    setError(null)
    setNewName(null)
  }, [item])

  useEffect(() => {
    if (!item) return
    let cancelled = false
    setFolders(null)
    setLoadError(null)
    listLibraryFolders(scope, path)
      .then((l) => {
        if (!cancelled) setFolders(l.folders)
      })
      .catch((err) => {
        if (cancelled) return
        // The mirrored folder may not exist in this root yet; the move creates
        // it. Browsing it is the same as browsing an empty folder.
        if (err instanceof ApiError && err.code() === 'folder_not_found') {
          setFolders([])
          return
        }
        setLoadError(libraryErrorText(err, 'This folder could not be read.'))
      })
    return () => {
      cancelled = true
    }
  }, [item, scope, path])

  if (!item) return null

  const from = { scope: scopeOf(item), path: pathOf(item) }
  const same = scope === from.scope && path === from.path
  const shares = from.scope === 'private' && scope === 'org'
  const unshares = from.scope === 'org' && scope === 'private'

  const move = async () => {
    setBusy(true)
    setError(null)
    try {
      const next = (await moveLibraryItem(item.id, { scope, path })) ?? null
      onMoved(next, from)
    } catch (err) {
      setError(libraryErrorText(err, 'The item could not be moved.'))
    } finally {
      setBusy(false)
    }
  }

  const createFolder = async () => {
    if (newName === null) return
    const problem = folderNameProblem(toSegment(newName), segmentsOf(path).length)
    if (problem) {
      setError(problem)
      return
    }
    const full = joinPath(path, toSegment(newName))
    setBusy(true)
    setError(null)
    try {
      await createLibraryFolder(scope, full)
      setNewName(null)
      setPath(full)
    } catch (err) {
      setError(libraryErrorText(err, 'The folder could not be created.'))
    } finally {
      setBusy(false)
    }
  }

  const crumbs = segmentsOf(path)
  const label = shares ? 'Move and share' : unshares ? 'Move and unshare' : 'Move here'

  return (
    <Modal open onClose={onClose} title={`Move “${item.name}”`} dismissible={!busy}>
      <div className={styles.body}>
        {canCrossRoots && (
          <div className={styles.roots} role="group" aria-label="Root">
            {(['private', 'org'] as LibraryScope[]).map((s) => (
              <button
                key={s}
                type="button"
                className={`${styles.root} ${scope === s ? styles.rootActive : ''}`}
                aria-pressed={scope === s}
                // The mirrored folder is the first guess: sharing Private/legal/
                // contracts/x lands it in Organisation/legal/contracts, created
                // if missing. The person can still pick another folder.
                onClick={() => {
                  setScope(s)
                  setPath(from.path)
                }}
              >
                {s === 'private' ? <PrivateGlyph size={14} /> : <OrgGlyph size={14} />}
                {SCOPE_LABEL[s]}
              </button>
            ))}
          </div>
        )}

        <nav className={styles.crumbs} aria-label="Destination folder">
          <button type="button" className={styles.crumb} onClick={() => setPath('')} aria-current={path === '' ? 'page' : undefined}>
            {SCOPE_LABEL[scope]}
          </button>
          {crumbs.map((seg, i) => {
            const p = crumbs.slice(0, i + 1).join('/')
            return (
              <span key={p} className={styles.crumbPair}>
                <span className={styles.crumbSep} aria-hidden="true">/</span>
                <button type="button" className={styles.crumb} onClick={() => setPath(p)} aria-current={p === path ? 'page' : undefined}>
                  {seg}
                </button>
              </span>
            )
          })}
        </nav>

        <ul className={styles.folderList} aria-label="Folders">
          {path !== '' && (
            <li>
              <button type="button" className={styles.folderRow} onClick={() => setPath(parentOf(path))}>
                <FolderGlyph size={14} />
                <span>.. ({nameOf(parentOf(path)) || SCOPE_LABEL[scope]})</span>
              </button>
            </li>
          )}
          {loadError && <li className={styles.folderNote}>{loadError}</li>}
          {folders === null && !loadError && <li className={styles.folderNote}>Loading…</li>}
          {folders?.map((f) => (
            <li key={f.path}>
              <button type="button" className={styles.folderRow} onClick={() => setPath(f.path)}>
                <FolderGlyph size={14} />
                <span className={styles.folderRowName}>{f.name}</span>
                <span className={styles.folderRowMeta}>
                  {f.items} {f.items === 1 ? 'item' : 'items'}
                </span>
                <svg className={styles.folderRowChevron} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <polyline points="9 18 15 12 9 6" />
                </svg>
              </button>
            </li>
          ))}
          {folders?.length === 0 && !loadError && (
            <li className={styles.folderNote}>{path && scope !== from.scope ? 'This folder will be created.' : 'No folders here.'}</li>
          )}
          <li>
            {newName === null ? (
              <button type="button" className={styles.folderNew} onClick={() => setNewName('')} disabled={busy}>
                + New folder
              </button>
            ) : (
              <form
                className={styles.folderNewForm}
                onSubmit={(e) => {
                  e.preventDefault()
                  void createFolder()
                }}
              >
                <input
                  className={styles.folderNewInput}
                  value={newName}
                  autoFocus
                  placeholder="folder-name"
                  aria-label="New folder name"
                  onChange={(e) => setNewName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Escape') {
                      e.preventDefault()
                      setNewName(null)
                    }
                  }}
                />
                <Button size="sm" variant="secondary" type="submit" disabled={busy}>
                  Create
                </Button>
              </form>
            )}
          </li>
        </ul>

        {shares && (
          <p className={styles.notice}>
            Moving it to Organisation shares it. Everyone at your organisation can read and attach it. Only you can edit it.
            {item.kind === 'tool' ? ' People who attach it call its server with the credential you saved.' : ''}
          </p>
        )}
        {unshares && (
          <p className={styles.notice}>
            Moving it to Private unshares it. Anyone who attached it loses it from their next message.
          </p>
        )}
        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        <div className={styles.actions}>
          {same && <span className={styles.hint}>It is already here.</span>}
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => void move()} disabled={busy || same}>
            {busy ? 'Moving…' : label}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
