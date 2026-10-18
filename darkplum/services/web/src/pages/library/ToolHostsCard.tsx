import { forwardRef, useCallback, useEffect, useId, useMemo, useState } from 'react'
import { getToolHosts, listSharedLibrary, updateToolHosts } from '../../lib/api'
import type { LibraryItem, ToolHostsSetting } from '../../types'
import { Button, Skeleton } from '../../components/ui'
import { useToast } from '../../components/Toast'
import { errorText, formatRelative, hostAllowed, hostOf } from './libraryText'
import styles from './LibraryDialogs.module.css'

/**
 * Admin only, at the top of the Tools tab: the hosts a tool server may live
 * on. Nothing calls a server that is not listed here, verify included.
 *
 * Beside the list it names the shared tools the draft would cut off, read
 * from the governance listing, so removing a host is not a silent outage for
 * whoever attached those tools.
 */
export const ToolHostsCard = forwardRef<HTMLHeadingElement, { refreshKey: number }>(function ToolHostsCard({ refreshKey }, headingRef) {
  const { toast } = useToast()
  const headingId = useId()
  const inputId = useId()
  const [setting, setSetting] = useState<ToolHostsSetting | null>(null)
  const [draft, setDraft] = useState<string[]>([])
  const [entry, setEntry] = useState('')
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [shared, setShared] = useState<LibraryItem[]>([])

  const load = useCallback(async () => {
    setLoadError(null)
    try {
      const s = await getToolHosts()
      setSetting(s)
      setDraft(s.hosts)
    } catch (err) {
      setLoadError(errorText(err, 'The list could not be loaded.'))
    }
  }, [])
  useEffect(() => { void load() }, [load])

  // The warning below is a courtesy; failing to read the shared list costs
  // only the warning.
  useEffect(() => {
    listSharedLibrary()
      .then((items) => setShared(items.filter((i) => i.kind === 'tool')))
      .catch(() => setShared([]))
  }, [refreshKey])

  const dirty = setting !== null && (draft.length !== setting.hosts.length || draft.some((h, i) => h !== setting.hosts[i]))

  const add = () => {
    const parts = entry
      .split(/[\s,]+/)
      // A pasted URL becomes its host name: scheme, user info, port and
      // path are dropped, because the server matches the host name alone.
      .map((h) =>
        h
          .trim()
          .toLowerCase()
          .replace(/^https?:\/\//, '')
          .replace(/\/.*$/, '')
          .replace(/^[^@]*@/, '')
          .replace(/:\d+$/, ''),
      )
      .filter(Boolean)
    if (parts.length === 0) return
    setDraft((prev) => [...prev, ...parts.filter((p) => !prev.includes(p))])
    setEntry('')
    setSaveError(null)
  }

  const save = async () => {
    setSaving(true)
    setSaveError(null)
    try {
      const s = await updateToolHosts(draft)
      setSetting(s)
      setDraft(s.hosts)
      toast('Allowed tool servers saved. They apply from the next call.', 'success')
    } catch (err) {
      setSaveError(errorText(err, 'The list could not be saved.'))
    } finally {
      setSaving(false)
    }
  }

  const blocked = useMemo(
    () =>
      shared.filter((t) => {
        const h = t.tool ? hostOf(t.tool.server_url) : null
        return h !== null && !hostAllowed(h, draft)
      }),
    [shared, draft],
  )

  return (
    <section className={styles.hostsCard} aria-labelledby={headingId}>
      <div className={styles.hostsHead}>
        <div>
          <h2 id={headingId} ref={headingRef} tabIndex={-1} className={styles.hostsTitle}>Allowed tool servers</h2>
          <p className={styles.hostsText}>
            A tool server must be listed here before any tool can call it, verify included. One host
            or wildcard per entry, such as <code className={styles.code}>mcp.corp.example</code> or{' '}
            <code className={styles.code}>*.svc.cluster.local</code>. Only administrators see this.
          </p>
        </div>
        {setting?.updated_at && (
          <span className={styles.hostsMeta} title={new Date(setting.updated_at).toLocaleString()}>
            Changed {formatRelative(setting.updated_at)}
          </span>
        )}
      </div>

      {loadError ? (
        <div className={styles.errorRow} role="alert">
          <span>{loadError}</span>
          <Button variant="secondary" size="sm" onClick={() => void load()}>Try again</Button>
        </div>
      ) : setting === null ? (
        <Skeleton height="64px" />
      ) : (
        <>
          {draft.length === 0 ? (
            <p className={styles.hostsEmpty}>No servers are allowed yet, so no tool can be verified or used in chat.</p>
          ) : (
            <ul className={styles.chips} aria-label="Allowed hosts">
              {draft.map((h) => (
                <li key={h} className={styles.chip}>
                  <span className={styles.mono}>{h}</span>
                  <button
                    type="button"
                    className={styles.chipRemove}
                    aria-label={`Remove ${h}`}
                    disabled={saving}
                    onClick={() => { setDraft((prev) => prev.filter((x) => x !== h)); setSaveError(null) }}
                  >
                    <svg width="12" height="12" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
                      <line x1="3" y1="3" x2="11" y2="11" />
                      <line x1="11" y1="3" x2="3" y2="11" />
                    </svg>
                  </button>
                </li>
              ))}
            </ul>
          )}

          <div className={styles.hostsAdd}>
            <label htmlFor={inputId} className={styles.srOnly}>Host to allow</label>
            <input
              id={inputId}
              className={`${styles.textInput} ${styles.mono}`}
              value={entry}
              placeholder="mcp.corp.example"
              autoComplete="off"
              spellCheck={false}
              disabled={saving}
              onChange={(e) => setEntry(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); add() } }}
            />
            <Button variant="secondary" size="sm" onClick={add} disabled={saving || entry.trim() === ''}>Add</Button>
          </div>

          {blocked.length > 0 && (
            <div className={styles.notice} role="status">
              {blocked.length === 1 ? 'One shared tool is' : `${blocked.length} shared tools are`} on a host this list does not allow, and cannot be used until it does:{' '}
              {blocked.map((t, i) => (
                <span key={t.id}>
                  {i > 0 && ', '}
                  {t.name} (<span className={styles.mono}>{t.tool ? hostOf(t.tool.server_url) : ''}</span>)
                </span>
              ))}
              .
            </div>
          )}

          {saveError && <p className={styles.error} role="alert">{saveError}</p>}

          <div className={styles.hostsActions}>
            {dirty && (
              <Button variant="ghost" size="sm" disabled={saving} onClick={() => { setDraft(setting.hosts); setSaveError(null) }}>
                Discard changes
              </Button>
            )}
            <Button variant="primary" size="sm" disabled={saving || !dirty} onClick={() => void save()}>
              {saving ? 'Saving…' : 'Save list'}
            </Button>
          </div>
        </>
      )}
    </section>
  )
})
