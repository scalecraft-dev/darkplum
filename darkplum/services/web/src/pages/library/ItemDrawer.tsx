import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import {
  adminDeleteLibraryItem,
  adminUnshareLibraryItem,
  createLibraryItem,
  deleteLibraryItem,
  exportLibraryItem,
  getLibraryItem,
  shareLibraryItem,
  updateLibraryItem,
} from '../../lib/api'
import { saveBlob } from '../../lib/download'
import type { LibraryItem, LibraryKind } from '../../types'
import { Button, CodeBlock, CopyButton, Input } from '../../components/ui'
import { useToast } from '../../components/Toast'
import { KindGlyph } from '../../components/ui/KindGlyph'
import { ToolSection } from './ToolSection'
import { ShareModal } from './ShareModal'
import { CredentialModal } from './CredentialModal'
import { useTwoClick } from './useTwoClick'
import {
  KIND_LABEL,
  KIND_NOUN,
  LIBRARY_BODY_MAX_BYTES,
  LIBRARY_DESC_MAX,
  LIBRARY_NAME_MAX,
  byteLength,
  errorText,
  formatFull,
  formatRelative,
  formatSize,
  ownerName,
  parseAllowedTools,
  runeLength,
  scopeText,
  toolBody,
} from './libraryText'
import styles from './ItemDrawer.module.css'

export type DrawerTarget =
  | { mode: 'new'; kind: LibraryKind }
  | { mode: 'open'; item: LibraryItem }

interface Props {
  target: DrawerTarget | null
  isAdmin: boolean
  onClose: () => void
  /** Something was created, changed or removed: the lists re-read. */
  onChanged: () => void
  /** Open another item in place of this one (a fresh copy). */
  onOpen: (item: LibraryItem) => void
  /** Admin: take the reader to the allowed-servers card. */
  onShowHosts: () => void
}

interface Draft {
  name: string
  description: string
  body: string
  serverUrl: string
  allowedTools: string
}

const EMPTY: Draft = { name: '', description: '', body: '', serverUrl: '', allowedTools: '' }

function draftOf(item: LibraryItem): Draft {
  return {
    name: item.name,
    description: item.description,
    body: item.kind === 'tool' ? '' : item.body,
    serverUrl: item.tool?.server_url ?? '',
    allowedTools: (item.tool?.allowed_tools ?? []).join(', '),
  }
}

function sameDraft(a: Draft, b: Draft): boolean {
  return (
    a.name === b.name &&
    a.description === b.description &&
    a.body === b.body &&
    a.serverUrl.trim() === b.serverUrl.trim() &&
    parseAllowedTools(a.allowedTools).join(',') === parseAllowedTools(b.allowedTools).join(',')
  )
}

function urlProblem(raw: string): string | null {
  const v = raw.trim()
  if (v === '') return null
  try {
    const u = new URL(v)
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'Use an http:// or https:// address.'
    if (u.username || u.password) return 'Leave the credential out of the address. Set it below once the tool is saved.'
    return null
  } catch {
    return 'Enter a full address, such as https://mcp.corp.example/mcp.'
  }
}

const remarkPlugins = [remarkGfm]
const rehypePlugins = [rehypeHighlight]
const mdComponents = {
  pre: CodeBlock,
  a({ children, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { children?: React.ReactNode }) {
    return <a target="_blank" rel="noopener noreferrer" {...props}>{children}</a>
  },
}

/**
 * One library item, right of the list. The owner edits it here; everyone
 * else reads it, and may copy it into their own library. An administrator
 * reading someone else's shared item also gets the two governance actions.
 */
export function ItemDrawer({ target, isAdmin, onClose, onChanged, onOpen, onShowHosts }: Props) {
  const { toast } = useToast()
  const ids = useId()
  const nameRef = useRef<HTMLInputElement>(null)
  const discardRef = useRef<HTMLButtonElement>(null)

  const [item, setItem] = useState<LibraryItem | null>(null)
  const [kind, setKind] = useState<LibraryKind>('skill')
  const [draft, setDraft] = useState<Draft>(EMPTY)
  const [saving, setSaving] = useState(false)
  const [busy, setBusy] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [askDiscard, setAskDiscard] = useState(false)
  const [shareOpen, setShareOpen] = useState(false)
  const [credentialOpen, setCredentialOpen] = useState(false)

  const open = target !== null
  const targetItemId = target?.mode === 'open' ? target.item.id : null

  // A new target resets the drawer to it, then reads the item again so what
  // is edited is what the server has now, not what the list had.
  useEffect(() => {
    setSaveError(null)
    setActionError(null)
    setAskDiscard(false)
    if (!target) return
    if (target.mode === 'new') {
      setItem(null)
      setKind(target.kind)
      setDraft(EMPTY)
      return
    }
    setItem(target.item)
    setKind(target.item.kind)
    setDraft(draftOf(target.item))
    let cancelled = false
    getLibraryItem(target.item.id)
      .then((fresh) => {
        if (cancelled) return
        setItem(fresh)
        setDraft((d) => (sameDraft(d, draftOf(target.item)) ? draftOf(fresh) : d))
      })
      .catch((err) => {
        if (!cancelled) setActionError(errorText(err, 'The latest version could not be read.'))
      })
    return () => { cancelled = true }
    // Keyed on the id: a re-render with the same item must not reset edits.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetItemId, target?.mode, target?.mode === 'new' ? target.kind : null])

  const editable = item === null || item.mine
  const baseline = useMemo(() => (item ? draftOf(item) : EMPTY), [item])
  const dirty = editable && !sameDraft(draft, baseline)

  const set = (patch: Partial<Draft>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setSaveError(null)
  }

  const requestClose = useCallback(() => {
    if (saving) return
    if (dirty) {
      setAskDiscard(true)
      return
    }
    onClose()
  }, [dirty, saving, onClose])

  useEffect(() => {
    if (askDiscard) discardRef.current?.focus()
  }, [askDiscard])

  // --- Validation, in the server's own units ---------------------------------
  const nameLen = runeLength(draft.name.trim())
  const descLen = runeLength(draft.description.trim())
  const bodyBytes = byteLength(draft.body)
  const bodyChars = runeLength(draft.body)
  const allowedList = parseAllowedTools(draft.allowedTools)
  const urlError = kind === 'tool' ? urlProblem(draft.serverUrl) : null
  const nameError = nameLen > LIBRARY_NAME_MAX ? `At most ${LIBRARY_NAME_MAX} characters.` : null
  const descError = descLen > LIBRARY_DESC_MAX ? `At most ${LIBRARY_DESC_MAX} characters.` : null
  const bodyOver = bodyBytes > LIBRARY_BODY_MAX_BYTES
  const valid =
    nameLen > 0 &&
    !nameError &&
    !descError &&
    (kind === 'tool' ? draft.serverUrl.trim() !== '' && !urlError : draft.body.trim() !== '' && !bodyOver)

  const save = async () => {
    if (!valid) return
    setSaving(true)
    setSaveError(null)
    const body = kind === 'tool' ? toolBody(draft.serverUrl, allowedList) : draft.body
    try {
      if (item === null) {
        const created = await createLibraryItem({
          kind,
          name: draft.name.trim(),
          description: draft.description.trim(),
          body,
        })
        setItem(created)
        setDraft(draftOf(created))
        toast(
          kind === 'tool'
            ? 'Tool saved. Verify it to see what it offers.'
            : `${KIND_LABEL[kind]} saved. Attach it to a chat from the composer.`,
          'success',
        )
      } else {
        const urlChanged = kind === 'tool' && item.tool?.server_url !== draft.serverUrl.trim()
        const updated = await updateLibraryItem(item.id, {
          name: draft.name.trim(),
          description: draft.description.trim(),
          body,
        })
        setItem(updated)
        setDraft(draftOf(updated))
        toast(urlChanged ? 'Saved. The address changed, so verify the server again.' : 'Saved. Used from the next turn.', 'success')
      }
      onChanged()
    } catch (err) {
      setSaveError(errorText(err, 'The item could not be saved.'))
    } finally {
      setSaving(false)
    }
  }

  const run = async (fn: () => Promise<void>, fallback: string) => {
    setBusy(true)
    setActionError(null)
    try {
      await fn()
    } catch (err) {
      setActionError(errorText(err, fallback))
    } finally {
      setBusy(false)
    }
  }

  const exportItem = () =>
    void run(async () => {
      if (!item) return
      const { blob, filename } = await exportLibraryItem(item.id)
      saveBlob(blob, filename)
    }, 'The item could not be exported.')

  const copyToMine = () =>
    void run(async () => {
      if (!item) return
      const source = await getLibraryItem(item.id)
      const copy = await createLibraryItem({
        kind: source.kind,
        name: source.name,
        description: source.description,
        body: source.body,
        visibility: 'private',
      })
      toast('Copied to your library. Only you can see the copy.', 'success')
      onChanged()
      onOpen(copy)
    }, 'The item could not be copied.')

  const del = useTwoClick(() =>
    void run(async () => {
      if (!item) return
      await deleteLibraryItem(item.id)
      toast(`${KIND_LABEL[item.kind]} deleted.`, 'success')
      onChanged()
      onClose()
    }, 'The item could not be deleted.'),
  )

  const unshare = useTwoClick(() =>
    void run(async () => {
      if (!item) return
      setItem(await shareLibraryItem(item.id, 'private'))
      toast('Unshared. Only you can see it now.', 'success')
      onChanged()
    }, 'The item could not be unshared.'),
  )

  const adminUnshare = useTwoClick(() =>
    void run(async () => {
      if (!item) return
      await adminUnshareLibraryItem(item.id)
      toast(`Unshared. It is private to ${ownerName(item)} again.`, 'success')
      onChanged()
      onClose()
    }, 'The item could not be unshared.'),
  )

  const adminRemove = useTwoClick(() =>
    void run(async () => {
      if (!item) return
      await adminDeleteLibraryItem(item.id)
      toast('Removed for everyone.', 'success')
      onChanged()
      onClose()
    }, 'The item could not be removed.'),
  )

  const title = item?.name ?? `New ${KIND_NOUN[kind]}`
  const governance = isAdmin && item !== null && !item.mine && item.visibility === 'org'

  // An armed "Confirm …" must not outlive the item it was armed for.
  useEffect(() => {
    del.disarm()
    unshare.disarm()
    adminUnshare.disarm()
    adminRemove.disarm()
    // Disarm on a change of item only; the hooks themselves are stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetItemId])

  return (
    <>
      <Dialog.Root open={open} onOpenChange={(v) => { if (!v) requestClose() }}>
        <Dialog.Portal>
          <Dialog.Overlay className={styles.overlay} />
          <Dialog.Content
            className={styles.drawer}
            onOpenAutoFocus={(e) => {
              // A new item starts at its name; an existing one at the close
              // button, so reading it does not begin with a caret in a field.
              if (target?.mode === 'new') {
                e.preventDefault()
                nameRef.current?.focus()
              }
            }}
          >
            <header className={styles.header}>
              <span className={styles.headerGlyph}><KindGlyph kind={kind} size={18} /></span>
              <div className={styles.headerText}>
                <Dialog.Title className={styles.title}>{title}</Dialog.Title>
                <Dialog.Description className={styles.subtitle}>
                  {item
                    ? <>{KIND_LABEL[item.kind]} · {scopeText(item)} · <time dateTime={item.updated_at} title={formatFull(item.updated_at)}>edited {formatRelative(item.updated_at)}</time></>
                    : `Only you can see it until you share it.`}
                </Dialog.Description>
              </div>
              <button type="button" className={styles.closeBtn} aria-label="Close" onClick={requestClose}>
                <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                  <line x1="3" y1="3" x2="11" y2="11" />
                  <line x1="11" y1="3" x2="3" y2="11" />
                </svg>
              </button>
            </header>

            {askDiscard && (
              <div className={styles.discard} role="alertdialog" aria-labelledby={`${ids}-discard`}>
                <span id={`${ids}-discard`}>You have unsaved changes.</span>
                <div className={styles.inlineActions}>
                  <Button ref={discardRef} variant="secondary" size="sm" onClick={() => setAskDiscard(false)}>Keep editing</Button>
                  <Button variant="danger" size="sm" onClick={() => { setAskDiscard(false); onClose() }}>Discard</Button>
                </div>
              </div>
            )}

            <div className={styles.scroll}>
              {editable ? (
                <form
                  id={`${ids}-form`}
                  className={styles.form}
                  onSubmit={(e) => { e.preventDefault(); void save() }}
                >
                  <Input
                    ref={nameRef}
                    label="Name"
                    value={draft.name}
                    onChange={(e) => set({ name: e.target.value })}
                    disabled={saving}
                    required
                    error={nameError ?? undefined}
                    aria-invalid={nameError ? true : undefined}
                  />
                  <Input
                    label="Description"
                    value={draft.description}
                    onChange={(e) => set({ description: e.target.value })}
                    placeholder={kind === 'tool' ? 'What the server is for' : 'When to use it, in one line'}
                    disabled={saving}
                    error={descError ?? undefined}
                    aria-invalid={descError ? true : undefined}
                  />

                  {kind === 'tool' ? (
                    <>
                      <Input
                        label="Server URL"
                        value={draft.serverUrl}
                        onChange={(e) => set({ serverUrl: e.target.value })}
                        placeholder="https://mcp.corp.example/mcp"
                        className={styles.monoInput}
                        spellCheck={false}
                        autoComplete="off"
                        inputMode="url"
                        disabled={saving}
                        required
                        error={urlError ?? undefined}
                        aria-invalid={urlError ? true : undefined}
                      />
                      <div className={styles.field}>
                        <Input
                          label="Allowed tools"
                          value={draft.allowedTools}
                          onChange={(e) => set({ allowedTools: e.target.value })}
                          placeholder="search_issues, get_issue"
                          className={styles.monoInput}
                          spellCheck={false}
                          autoComplete="off"
                          disabled={saving}
                          aria-describedby={`${ids}-allowed-hint`}
                        />
                        <span id={`${ids}-allowed-hint`} className={styles.hint}>
                          Optional, comma-separated. Leave empty to let chat call every tool the server offers.
                        </span>
                      </div>
                    </>
                  ) : (
                    <div className={styles.field}>
                      <label htmlFor={`${ids}-body`} className={styles.label}>
                        {kind === 'skill' ? 'Instructions' : 'Content'}
                      </label>
                      <textarea
                        id={`${ids}-body`}
                        className={styles.editor}
                        value={draft.body}
                        onChange={(e) => set({ body: e.target.value })}
                        rows={16}
                        spellCheck={false}
                        disabled={saving}
                        required
                        aria-invalid={bodyOver ? true : undefined}
                        aria-describedby={`${ids}-count`}
                        placeholder={kind === 'skill' ? 'Markdown. Tell the model how to do the task.' : 'Markdown. Background the model should know.'}
                      />
                      <span
                        id={`${ids}-count`}
                        className={`${styles.count} ${bodyOver ? styles.countOver : bodyBytes > LIBRARY_BODY_MAX_BYTES * 0.9 ? styles.countNear : ''}`}
                      >
                        {bodyChars.toLocaleString()} characters, {formatSize(bodyBytes)} of 64 KB
                        {bodyOver ? '. Shorten it to save.' : ''}
                      </span>
                    </div>
                  )}

                  {saveError && <div className={styles.alert} role="alert"><span>{saveError}</span></div>}
                </form>
              ) : item ? (
                <div className={styles.readOnly}>
                  {item.description && <p className={styles.description}>{item.description}</p>}
                  {item.kind === 'tool' ? (
                    <dl className={styles.facts}>
                      <dt>Server URL</dt>
                      <dd className={styles.factWithCopy}>
                        <span className={styles.mono}>{item.tool?.server_url}</span>
                        {item.tool?.server_url && <CopyButton value={item.tool.server_url} label="Copy" />}
                      </dd>
                      <dt>Allowed tools</dt>
                      <dd>
                        {item.tool?.allowed_tools && item.tool.allowed_tools.length > 0
                          ? <span className={styles.mono}>{item.tool.allowed_tools.join(', ')}</span>
                          : 'Every tool the server offers'}
                      </dd>
                    </dl>
                  ) : (
                    <div className={styles.markdown}>
                      <ReactMarkdown remarkPlugins={remarkPlugins} rehypePlugins={rehypePlugins} components={mdComponents}>
                        {item.body}
                      </ReactMarkdown>
                    </div>
                  )}
                </div>
              ) : null}

              {item && item.kind === 'tool' && (
                <ToolSection
                  item={item}
                  isAdmin={isAdmin}
                  urlUnsaved={editable && draft.serverUrl.trim() !== (item.tool?.server_url ?? '')}
                  onItem={(next) => { setItem(next); onChanged() }}
                  onSetCredential={() => setCredentialOpen(true)}
                  onShowHosts={() => {
                    // Same guard as the close button: unsaved edits are
                    // never dropped on the way to the allowed-servers card.
                    if (dirty) {
                      setAskDiscard(true)
                      return
                    }
                    onClose()
                    onShowHosts()
                  }}
                />
              )}
              {item === null && kind === 'tool' && (
                <p className={styles.hint}>Save the tool to verify its server and set a credential.</p>
              )}

              {item?.mine && (
                <section className={styles.section} aria-labelledby={`${ids}-sharing`}>
                  <h3 id={`${ids}-sharing`} className={styles.sectionTitle}>Sharing</h3>
                  {item.visibility === 'org' ? (
                    <>
                      <p className={styles.sectionText}>
                        Everyone in your organisation can read and attach this. Only you can edit it.
                      </p>
                      <div className={styles.inlineActions}>
                        <Button variant="secondary" size="sm" onClick={unshare.click} disabled={busy}>
                          {unshare.armed ? 'Confirm unshare' : 'Unshare'}
                        </Button>
                      </div>
                    </>
                  ) : (
                    <>
                      <p className={styles.sectionText}>Only you can see this.</p>
                      <div className={styles.inlineActions}>
                        <Button variant="secondary" size="sm" onClick={() => setShareOpen(true)} disabled={busy || dirty}>
                          Share with organisation
                        </Button>
                        {dirty && <span className={styles.hint}>Save your changes first.</span>}
                      </div>
                    </>
                  )}
                </section>
              )}

              {governance && item && (
                <section className={styles.section} aria-labelledby={`${ids}-admin`}>
                  <h3 id={`${ids}-admin`} className={styles.sectionTitle}>Administration</h3>
                  <p className={styles.sectionText}>
                    Unshare makes it private to {ownerName(item)} again. Remove deletes it for everyone,
                    including {ownerName(item)}. Both are recorded in the activity log.
                  </p>
                  <div className={styles.inlineActions}>
                    <Button variant="secondary" size="sm" onClick={adminUnshare.click} disabled={busy}>
                      {adminUnshare.armed ? 'Confirm unshare' : 'Unshare'}
                    </Button>
                    <Button variant="danger" size="sm" onClick={adminRemove.click} disabled={busy}>
                      {adminRemove.armed ? 'Confirm remove' : 'Remove'}
                    </Button>
                  </div>
                </section>
              )}

              {actionError && <div className={styles.alert} role="alert"><span>{actionError}</span></div>}
            </div>

            <footer className={styles.footer}>
              {item?.mine && (
                <Button variant="danger" size="sm" onClick={del.click} disabled={busy || saving}>
                  {del.armed ? 'Confirm delete' : 'Delete'}
                </Button>
              )}
              <div className={styles.footerEnd}>
                {item && !item.mine && item.kind !== 'tool' && (
                  <Button variant="secondary" size="sm" onClick={copyToMine} disabled={busy}>
                    Copy to my library
                  </Button>
                )}
                {item && (
                  <Button variant="secondary" size="sm" onClick={exportItem} disabled={busy || dirty} title={dirty ? 'Save your changes first' : undefined}>
                    Export {item.kind === 'tool' ? '.json' : '.md'}
                  </Button>
                )}
                {editable && (
                  <Button
                    type="submit"
                    form={`${ids}-form`}
                    variant="primary"
                    size="sm"
                    disabled={saving || !valid || (item !== null && !dirty)}
                  >
                    {saving ? 'Saving…' : item === null ? `Save ${KIND_NOUN[kind]}` : 'Save'}
                  </Button>
                )}
              </div>
            </footer>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      <ShareModal
        item={shareOpen ? item : null}
        onClose={() => setShareOpen(false)}
        onShared={(next) => {
          setItem(next)
          setShareOpen(false)
          toast('Shared with your organisation.', 'success')
          onChanged()
        }}
      />
      <CredentialModal
        item={credentialOpen ? item : null}
        onClose={() => setCredentialOpen(false)}
        onSaved={(next) => {
          setItem(next)
          setCredentialOpen(false)
          toast(`Credential saved, ends in ••••${next.tool?.credential.last4 ?? ''}.`, 'success')
          onChanged()
        }}
      />
    </>
  )
}
