import { useState } from 'react'
import { ApiError, getLibraryItem, removeLibraryToolCredential, verifyLibraryTool } from '../../lib/api'
import type { LibraryItem } from '../../types'
import { Badge, Button } from '../../components/ui'
import { useToast } from '../../components/Toast'
import { useTwoClick } from './useTwoClick'
import { credentialErrorText, formatFull, formatRelative, hostOf, ownerName, verifyErrorText } from './libraryText'
import styles from './ItemDrawer.module.css'

interface Props {
  item: LibraryItem
  isAdmin: boolean
  /** The address in the form differs from the saved one: verify would check
   *  the old server, so it waits for a save. */
  urlUnsaved: boolean
  onItem: (item: LibraryItem) => void
  onSetCredential: () => void
  onShowHosts: () => void
}

/** What the server offers and whether it can be reached: verification, the
 *  catalogue it returned, and the credential. Owner controls; everyone else
 *  reads the same facts. */
export function ToolSection({ item, isAdmin, urlUnsaved, onItem, onSetCredential, onShowHosts }: Props) {
  const { toast } = useToast()
  const tool = item.tool
  const [verifying, setVerifying] = useState(false)
  const [verifyError, setVerifyError] = useState<{ text: string; hostRefused: boolean } | null>(null)
  const [announce, setAnnounce] = useState('')
  const [credBusy, setCredBusy] = useState(false)
  const [credError, setCredError] = useState<string | null>(null)

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

  if (!tool) return null

  const verify = async () => {
    setVerifying(true)
    setVerifyError(null)
    setAnnounce('')
    try {
      const updated = await verifyLibraryTool(item.id)
      onItem(updated)
      const n = updated.tool?.catalog.length ?? 0
      setAnnounce(`Verified. The server offers ${n} ${n === 1 ? 'tool' : 'tools'}.`)
    } catch (err) {
      const text = verifyErrorText(err, { isAdmin, credentialSet: tool.credential.set, serverUrl: tool.server_url })
      const hostRefused = err instanceof ApiError && err.code() === 'tool_host_not_allowed'
      setVerifyError({ text, hostRefused })
      setAnnounce(text)
    } finally {
      setVerifying(false)
    }
  }

  const verified = tool.catalog_at !== null
  const allowed = tool.allowed_tools ?? []
  const host = hostOf(tool.server_url) ?? tool.server_url

  return (
    <>
      <section className={styles.section} aria-labelledby={`server-${item.id}`}>
        <div className={styles.sectionHead}>
          <h3 id={`server-${item.id}`} className={styles.sectionTitle}>Server</h3>
          {verified ? (
            <Badge variant="success" bordered>Verified</Badge>
          ) : (
            <Badge variant="neutral" dashed>Not verified</Badge>
          )}
        </div>

        {verified ? (
          <p className={styles.sectionText}>
            {tool.server_name ? <>{tool.server_name} on <span className={styles.mono}>{host}</span></> : <span className={styles.mono}>{host}</span>}
            , verified{' '}
            <time dateTime={tool.catalog_at ?? undefined} title={tool.catalog_at ? formatFull(tool.catalog_at) : undefined}>
              {tool.catalog_at ? formatRelative(tool.catalog_at) : ''}
            </time>
            .
          </p>
        ) : item.mine ? (
          <p className={styles.sectionText}>Not verified yet. Verify to see what this server offers and to use it in chat.</p>
        ) : (
          <p className={styles.sectionText}>Not verified yet. {ownerName(item)} has to verify it before anyone can use it in chat.</p>
        )}

        {item.mine && (
          <div className={styles.inlineActions}>
            <Button variant="secondary" size="sm" onClick={() => void verify()} disabled={verifying || urlUnsaved}>
              {verifying ? 'Verifying…' : verified ? 'Verify again' : 'Verify server'}
            </Button>
            {urlUnsaved && <span className={styles.hint}>Save the new address first.</span>}
          </div>
        )}

        <p className={styles.srOnly} role="status">{announce}</p>
        {verifyError && (
          <div className={styles.alert} role="alert">
            <span>{verifyError.text}</span>
            {verifyError.hostRefused && isAdmin && (
              <Button variant="secondary" size="sm" onClick={onShowHosts}>Show allowed servers</Button>
            )}
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
