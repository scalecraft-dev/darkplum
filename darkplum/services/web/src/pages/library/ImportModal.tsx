import { useEffect, useId, useRef, useState } from 'react'
import { importLibraryItem } from '../../lib/api'
import type { LibraryImportPreview, LibraryKind, LibraryVisibility } from '../../types'
import { Button, Modal } from '../../components/ui'
import { KindGlyph } from '../../components/ui/KindGlyph'
import { KIND_LABEL, KIND_PLURAL, errorText, formatSize } from './libraryText'
import styles from './LibraryDialogs.module.css'

/** Files this large are not read at all: the body cap is 64 KiB, and a
 *  front-matter block does not take a file anywhere near this. */
const READ_LIMIT_BYTES = 1024 * 1024

type Row = {
  key: string
  filename: string
  content: string | null
  /** The dry run's answer, or null while it is out. */
  preview: LibraryImportPreview | null
  /** Why this file will not come in (read, dry run or the real import). */
  error: string | null
  status: 'checking' | 'ready' | 'failed' | 'imported'
}

interface Props {
  open: boolean
  /** The tab it was opened from. A `kind:` in a file's front matter wins. */
  kind: LibraryKind
  onClose: () => void
  /** Called once the imports have run, with how many came in. */
  onImported: (count: number) => void
}

function readFile(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(new Error('The file could not be read.'))
    reader.readAsText(file)
  })
}

export function ImportModal({ open, kind, onClose, onImported }: Props) {
  const inputId = useId()
  const inputRef = useRef<HTMLInputElement>(null)
  const [rows, setRows] = useState<Row[]>([])
  const [visibility, setVisibility] = useState<LibraryVisibility>('private')
  const [phase, setPhase] = useState<'pick' | 'importing' | 'done'>('pick')
  const [summary, setSummary] = useState('')

  useEffect(() => {
    if (!open) return
    setRows([])
    setVisibility('private')
    setPhase('pick')
    setSummary('')
    if (inputRef.current) inputRef.current.value = ''
  }, [open])

  const update = (key: string, patch: Partial<Row>) =>
    setRows((prev) => prev.map((r) => (r.key === key ? { ...r, ...patch } : r)))

  const onFiles = async (list: FileList | null) => {
    if (!list || list.length === 0) return
    const files = [...list]
    const fresh: Row[] = files.map((f, i) => ({
      key: `${Date.now()}-${i}-${f.name}`,
      filename: f.name,
      content: null,
      preview: null,
      error: null,
      status: 'checking',
    }))
    setRows(fresh)
    setPhase('pick')
    setSummary('')
    // One at a time: the server takes one file per call, and the preview
    // fills in top to bottom rather than all at once.
    for (let i = 0; i < files.length; i++) {
      const file = files[i]
      const row = fresh[i]
      if (file.size > READ_LIMIT_BYTES) {
        update(row.key, { status: 'failed', error: `This file is ${formatSize(file.size)}. A library item holds at most 64 KB.` })
        continue
      }
      try {
        const content = await readFile(file)
        const preview = await importLibraryItem({ filename: file.name, content, kind }, true)
        update(row.key, { content, preview, status: 'ready' })
      } catch (err) {
        update(row.key, { status: 'failed', error: errorText(err, 'This file cannot be imported.') })
      }
    }
  }

  const ready = rows.filter((r) => r.status === 'ready')
  const checking = rows.some((r) => r.status === 'checking')

  const confirm = async () => {
    setPhase('importing')
    let imported = 0
    let failed = rows.filter((r) => r.status === 'failed').length
    for (const row of ready) {
      try {
        await importLibraryItem({ filename: row.filename, content: row.content ?? '', kind, visibility })
        imported++
        update(row.key, { status: 'imported' })
      } catch (err) {
        failed++
        update(row.key, { status: 'failed', error: errorText(err, 'The import failed.') })
      }
    }
    const text = failed > 0 ? `${imported} imported, ${failed} failed.` : `${imported} imported.`
    setSummary(text)
    setPhase('done')
    onImported(imported)
  }

  const busy = phase === 'importing'

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Import"
      description={kind === 'tool'
        ? 'Choose the .json files darkplum exports for tools. Each file is checked before anything is saved.'
        : `Choose .md files to bring in as ${KIND_PLURAL[kind]}. Each file is checked before anything is saved.`}
      dismissible={!busy}
    >
      <div className={styles.body}>
        <div className={styles.filePick}>
          <label htmlFor={inputId} className={styles.fileLabel}>Files</label>
          <input
            ref={inputRef}
            id={inputId}
            type="file"
            multiple
            accept=".md,.markdown,.json,text/markdown,application/json"
            className={styles.fileInput}
            disabled={busy}
            onChange={(e) => void onFiles(e.target.files)}
          />
        </div>

        {rows.length > 0 && (
          <ul className={styles.previewList} aria-label="Files to import" aria-busy={checking || busy}>
            {rows.map((r) => (
              <li key={r.key} className={styles.previewRow}>
                <span className={styles.previewGlyph}>
                  {r.preview ? <KindGlyph kind={r.preview.kind} size={14} /> : null}
                </span>
                <div className={styles.previewMain}>
                  <div className={styles.previewName}>
                    {r.preview?.name ?? r.filename}
                    {r.preview && (
                      <span className={styles.previewMeta}>
                        {KIND_LABEL[r.preview.kind]} · {formatSize(r.preview.size_bytes)}
                      </span>
                    )}
                  </div>
                  <div className={styles.previewFile}>{r.filename}</div>
                  {r.preview && r.preview.dropped.length > 0 && r.status !== 'failed' && (
                    <div className={styles.previewDropped}>
                      Will drop: <span className={styles.mono}>{r.preview.dropped.join(', ')}</span>
                    </div>
                  )}
                  {r.error && <div className={styles.previewError}>{r.error}</div>}
                </div>
                <span className={`${styles.previewStatus} ${r.status === 'failed' ? styles.statusFailed : r.status === 'imported' ? styles.statusDone : ''}`}>
                  {r.status === 'checking' ? 'Checking…' : r.status === 'ready' ? (busy ? 'Waiting' : 'Ready') : r.status === 'imported' ? 'Imported' : 'Not imported'}
                </span>
              </li>
            ))}
          </ul>
        )}

        {rows.length > 0 && phase !== 'done' && (
          <fieldset className={styles.choice} disabled={busy}>
            <legend className={styles.fileLabel}>Who can see them</legend>
            <label className={styles.radio}>
              <input type="radio" name={`${inputId}-vis`} checked={visibility === 'private'} onChange={() => setVisibility('private')} />
              <span>Only you</span>
            </label>
            <label className={styles.radio}>
              <input type="radio" name={`${inputId}-vis`} checked={visibility === 'org'} onChange={() => setVisibility('org')} />
              <span>Everyone in your organisation. Only you can edit them.</span>
            </label>
          </fieldset>
        )}

        <p className={styles.summary} role="status">{summary}</p>

        <div className={styles.actions}>
          {phase === 'done' ? (
            <Button variant="primary" onClick={onClose}>Done</Button>
          ) : (
            <>
              <Button variant="secondary" onClick={onClose} disabled={busy}>Cancel</Button>
              <Button
                variant="primary"
                onClick={() => void confirm()}
                disabled={busy || checking || ready.length === 0}
              >
                {busy ? 'Importing…' : ready.length > 1 ? `Import ${ready.length} files` : 'Import'}
              </Button>
            </>
          )}
        </div>
      </div>
    </Modal>
  )
}
