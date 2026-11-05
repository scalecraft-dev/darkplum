import { useEffect, useRef, useState } from 'react'
import { ApiError, getLibraryItem, getToolHosts, listSharedLibrary, removeLibraryToolCredential, updateToolHosts, verifyLibraryTool } from '../../lib/api'
import type { LibraryItem } from '../../types'
import { Badge, Button } from '../../components/ui'
import { useToast } from '../../components/Toast'
import { useTwoClick } from './useTwoClick'
import { credentialErrorText, errorText, formatFull, formatRelative, hostAllowed, hostOf, ownerName, verifyErrorText } from './libraryText'
import styles from './ItemDrawer.module.css'

interface Props {
  item: LibraryItem
  isAdmin: boolean
  /** The address in the form differs from the saved one: a check would
   *  reach the old server, so it waits for a save. */
  urlUnsaved: boolean
  /** Each change past zero checks the server: the drawer bumps it after a
   *  save that set or changed this item's server, and only for this item. */
  checkRun?: number
  /** Admin only: the one copy of the allowed-server list, owned by the
   *  drawer so the form and this section never disagree. null until read,
   *  and for everyone else. */
  hosts: string[] | null
  onHosts: (hosts: string[]) => void
  onItem: (item: LibraryItem) => void
  onSetCredential: () => void
}

/** What the server offers and whether it can be reached: the check, the
 *  catalogue it returned, and the credential. Owner controls; everyone else
 *  reads the same facts. An administrator also allows the server here, or
 *  stops allowing it, for every tool on it. */
export function ToolSection({ item, isAdmin, urlUnsaved, checkRun = 0, hosts, onHosts, onItem, onSetCredential }: Props) {
  const { toast } = useToast()
  const tool = item.tool
  const [verifying, setVerifying] = useState(false)
  const [verifyError, setVerifyError] = useState<{ text: string; hostRefused: boolean } | null>(null)
  const [announce, setAnnounce] = useState('')
  const [credBusy, setCredBusy] = useState(false)
  const [credError, setCredError] = useState<string | null>(null)
  const [hostBusy, setHostBusy] = useState<'allow' | 'remove' | null>(null)
  const [hostError, setHostError] = useState<string | null>(null)
  /** Other shared tools on this host, read when the remove is armed: the
   *  people who attached them lose the tool. null while unknown. */
  const [sharedOnHost, setSharedOnHost] = useState<number | null>(null)
  const host = hostOf(tool?.server_url ?? '') ?? ''

  /** Allow this server for every tool. The list is read again right before
   *  it is written: another administrator may have changed it. */
  const allowHost = async (): Promise<boolean> => {
    setHostBusy('allow')
    setHostError(null)
    try {
      const current = (await getToolHosts()).hosts
      const next = hostAllowed(host, current) ? current : (await updateToolHosts([...current, host])).hosts
      onHosts(next)
      toast(`${host} allowed for every tool.`, 'success')
      return true
    } catch (err) {
      setHostError(errorText(err, `${host} could not be allowed. Try again.`))
      return false
    } finally {
      setHostBusy(null)
    }
  }

  const disallow = useTwoClick(() => {
    void (async () => {
      setHostBusy('remove')
      setHostError(null)
      try {
        const current = (await getToolHosts()).hosts
        const next = (await updateToolHosts(current.filter((h) => h !== host))).hosts
        onHosts(next)
        toast(
          hostAllowed(host, next)
            ? `${host} removed, but a pattern in the list still allows it.`
            : `${host} is no longer an allowed server. Tools on it are refused from the next call.`,
          'success',
        )
      } catch (err) {
        setHostError(errorText(err, `${host} could not be removed. Try again.`))
      } finally {
        setHostBusy(null)
      }
    })()
  })
  useEffect(() => {
    if (!disallow.armed || !isAdmin) return
    let cancelled = false
    setSharedOnHost(null)
    listSharedLibrary()
      .then((items) => {
        if (!cancelled) setSharedOnHost(items.filter((i) => i.kind === 'tool' && i.id !== item.id && hostOf(i.tool?.server_url ?? '') === host).length)
      })
      .catch(() => { /* stays unknown, and says so */ })
    return () => { cancelled = true }
  }, [disallow.armed, isAdmin, item.id, host])

  const removeCredential = useTwoClick(() => {
    void (async () => {
      setCredBusy(true)
      setCredError(null)
      try {
        await removeLibraryToolCredential(item.id)
        onItem(await getLibraryItem(item.id))
        toast('Credential removed.', 'success')
      } catch (err) {
        setCredError(credentialErrorText(err))
      } finally {
        setCredBusy(false)
      }
    })()
  })

  const verify = async () => {
    if (!tool) return
    setVerifying(true)
    setVerifyError(null)
    setAnnounce('')
    try {
      const updated = await verifyLibraryTool(item.id)
      onItem(updated)
      const n = updated.tool?.catalog.length ?? 0
      setAnnounce(`Checked. The server offers ${n} ${n === 1 ? 'tool' : 'tools'}.`)
    } catch (err) {
      const text = verifyErrorText(err, { isAdmin, credentialSet: tool.credential.set, serverUrl: tool.server_url })
      const hostRefused = err instanceof ApiError && err.code() === 'tool_host_not_allowed'
      setVerifyError({ text, hostRefused })
      setAnnounce(text)
      // The list the drawer holds may be missing or behind: the refusal is
      // the truth, so read the list again and the Allow button follows.
      if (hostRefused && isAdmin) getToolHosts().then((s) => onHosts(s.hosts)).catch(() => {})
    } finally {
      setVerifying(false)
    }
  }

  // A save that set the server checks it here, so saving is the only step.
  // The counter is this item's alone (the drawer keys this section by item
  // id, so a fresh instance starts at zero with it).
  const lastRun = useRef(0)
  useEffect(() => {
    if (checkRun > lastRun.current) {
      lastRun.current = checkRun
      void verify()
    }
    // verify reads the current item; the run counter is the trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [checkRun])

  if (!tool) return null

  const allowAndVerify = async () => {
    if (await allowHost()) await verify()
  }

  const verified = tool.catalog_at !== null
  const allowed = tool.allowed_tools ?? []
  const hostExact = hosts !== null && hosts.includes(host)
  const hostByPattern = hosts !== null && !hostExact && hostAllowed(host, hosts)
  const hostStanding = hosts === null || !host ? null : hostExact
    ? `${host} is an allowed server for every tool.`
    : hostByPattern
      ? `${host} is allowed by the pattern ${hosts.find((p) => hostAllowed(host, [p])) ?? ''}. Patterns are changed with PUT /api/v1/settings/tool-hosts.`
      : `${host} is not an allowed server; calls to it are refused.`
  const hostNotAllowed = hosts !== null && !!host && !hostExact && !hostByPattern
  const busy = hostBusy !== null || verifying

  return (
    <>
      <section className={styles.section} aria-labelledby={`server-${item.id}`}>
        <div className={styles.sectionHead}>
          <h3 id={`server-${item.id}`} className={styles.sectionTitle}>Server</h3>
          {verified ? (
            <Badge variant="success" bordered>Checked</Badge>
          ) : (
            <Badge variant="neutral" dashed>Not checked</Badge>
          )}
        </div>

        {verified ? (
          <p className={styles.sectionText}>
            {tool.server_name ? <>{tool.server_name} on <span className={styles.mono}>{host}</span></> : <span className={styles.mono}>{host}</span>}
            , checked{' '}
            <time dateTime={tool.catalog_at ?? undefined} title={tool.catalog_at ? formatFull(tool.catalog_at) : undefined}>
              {tool.catalog_at ? formatRelative(tool.catalog_at) : ''}
            </time>
            .
          </p>
        ) : item.mine ? (
          <p className={styles.sectionText}>Not checked yet. Check the server to see what it offers and to use it in chat.</p>
        ) : (
          <p className={styles.sectionText}>Not checked yet. {ownerName(item)} has to check it before anyone can use it in chat.</p>
        )}

        {item.mine && (
          <div className={styles.inlineActions}>
            <Button variant="secondary" size="sm" onClick={() => void verify()} disabled={busy || urlUnsaved}>
              {verifying ? 'Checking…' : verified ? 'Check again' : 'Check server'}
            </Button>
            {urlUnsaved && <span className={styles.hint}>Save the new address first.</span>}
          </div>
        )}

        {isAdmin && hostStanding && (
          <>
            <p className={styles.sectionText}>{hostStanding}</p>
            {hostNotAllowed && (
              <div className={styles.inlineActions}>
                <Button variant="primary" size="sm" onClick={() => void (item.mine ? allowAndVerify() : allowHost())} disabled={busy}>
                  {hostBusy === 'allow' ? 'Allowing…' : item.mine ? `Allow ${host} and check` : `Allow ${host}`}
                </Button>
                {!item.mine && <span className={styles.hint}>{ownerName(item)} can then check it.</span>}
              </div>
            )}
            {hostExact && (
              <div className={styles.inlineActions}>
                <Button variant="danger" size="sm" onClick={disallow.click} disabled={busy}>
                  {hostBusy === 'remove' ? 'Removing…' : disallow.armed ? `Confirm: stop allowing ${host}` : `Stop allowing ${host}`}
                </Button>
                {disallow.armed && (
                  <span className={styles.hint}>
                    {sharedOnHost === null
                      ? 'Looking up which shared tools use this server…'
                      : sharedOnHost === 0
                        ? 'No other shared tool uses this server. Private tools on it are not counted.'
                        : `${sharedOnHost} other shared ${sharedOnHost === 1 ? 'tool uses' : 'tools use'} this server and would stop working.`}
                  </span>
                )}
              </div>
            )}
            {hostError && <div className={styles.alert} role="alert"><span>{hostError}</span></div>}
          </>
        )}

        <p className={styles.srOnly} role="status">{announce}</p>
        {verifyError && !(verifyError.hostRefused && isAdmin && hostNotAllowed) && (
          <div className={styles.alert} role="alert">
            <span>{verifyError.text}</span>
          </div>
        )}

        {verified && (
          tool.catalog.length === 0 ? (
            <p className={styles.hint}>The server answered but offers no tools.</p>
          ) : (
            <ul className={styles.catalog} aria-label="Tools this server offers">
              {tool.catalog.map((t) => {
                const off = allowed.length > 0 && !allowed.includes(t.name)
                return (
                  <li key={t.name} className={`${styles.catalogRow} ${off ? styles.catalogOff : ''}`}>
                    <div className={styles.catalogName}>
                      <span className={styles.mono}>{t.name}</span>
                      {off && <Badge variant="neutral">Not in allowed tools</Badge>}
                    </div>
                    {t.description && <p className={styles.catalogDesc}>{t.description}</p>}
                  </li>
                )
              })}
            </ul>
          )
        )}
      </section>

      <section className={styles.section} aria-labelledby={`cred-${item.id}`}>
        <h3 id={`cred-${item.id}`} className={styles.sectionTitle}>Credential</h3>
        {item.mine ? (
          <>
            <p className={styles.sectionText}>
              {tool.credential.set ? (
                <>Credential set, ends in <span className={styles.mono}>••••{tool.credential.last4 ?? ''}</span>.</>
              ) : (
                'No credential. Calls reach the server without one.'
              )}
            </p>
            <div className={styles.inlineActions}>
              <Button variant="secondary" size="sm" onClick={onSetCredential} disabled={credBusy}>
                {tool.credential.set ? 'Replace credential' : 'Set credential'}
              </Button>
              {tool.credential.set && (
                <Button variant="danger" size="sm" onClick={removeCredential.click} disabled={credBusy}>
                  {credBusy ? 'Removing…' : removeCredential.armed ? 'Confirm remove' : 'Remove'}
                </Button>
              )}
            </div>
            {credError && <div className={styles.alert} role="alert"><span>{credError}</span></div>}
          </>
        ) : (
          <p className={styles.sectionText}>
            {tool.credential.set
              ? `${ownerName(item)} saved a credential for this server. Your calls use it.`
              : 'No credential is saved. Calls reach the server without one.'}
          </p>
        )}
      </section>
    </>
  )
}
