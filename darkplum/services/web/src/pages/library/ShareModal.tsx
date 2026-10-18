import { useState } from 'react'
import { shareLibraryItem } from '../../lib/api'
import type { LibraryItem } from '../../types'
import { Button, Modal } from '../../components/ui'
import { errorText } from './libraryText'
import styles from './LibraryDialogs.module.css'

interface Props {
  item: LibraryItem | null
  onClose: () => void
  onShared: (item: LibraryItem) => void
}

/** Sharing is the one library action that reaches other people, so it is
 *  said in full before it happens. Unsharing is a plain two-click action. */
export function ShareModal({ item, onClose, onShared }: Props) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const share = async () => {
    if (!item) return
    setBusy(true)
    setError(null)
    try {
      onShared(await shareLibraryItem(item.id, 'org'))
    } catch (err) {
      setError(errorText(err, 'The item could not be shared.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={item !== null}
      onClose={() => { setError(null); onClose() }}
      title={item ? `Share ${item.name}` : 'Share'}
      dismissible={!busy}
    >
      <div className={styles.body}>
        <p className={styles.text}>
          Everyone in your organisation can read and attach this. Only you can edit it.
          Administrators can unshare it.
        </p>
        {item?.kind === 'tool' && (
          <p className={styles.notice}>
            People who attach this tool call its server with the credential you saved.
          </p>
        )}
        {error && <p className={styles.error} role="alert">{error}</p>}
        <div className={styles.actions}>
          <Button variant="secondary" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" onClick={() => void share()} disabled={busy}>
            {busy ? 'Sharing…' : 'Share with organisation'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
