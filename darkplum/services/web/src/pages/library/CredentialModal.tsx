import { useEffect, useRef, useState } from 'react'
import { setLibraryToolCredential } from '../../lib/api'
import type { LibraryItem } from '../../types'
import { Button, Input, Modal } from '../../components/ui'
import { credentialErrorText } from './libraryText'
import styles from './LibraryDialogs.module.css'

interface Props {
  item: LibraryItem | null
  onClose: () => void
  onSaved: (item: LibraryItem) => void
}

/**
 * The bearer token a tool server needs. The vendor-key rules apply: an
 * uncontrolled password input read by ref, cleared on every exit, never in
 * React state and never echoed back.
 */
export function CredentialModal({ item, onClose, onSaved }: Props) {
  const ref = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const open = item !== null

  const clear = () => { if (ref.current) ref.current.value = '' }
  useEffect(() => {
    if (open) setError(null)
    return clear
  }, [open])

  const close = () => {
    clear()
    setError(null)
    onClose()
  }

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!item) return
    const token = ref.current?.value ?? ''
    if (token.trim() === '') {
      setError('Paste the token the server expects.')
      ref.current?.focus()
      return
    }
    if (/\s/.test(token.trim())) {
      setError('A token has no spaces or line breaks in it. Check what was pasted.')
      ref.current?.focus()
      return
    }
    setBusy(true)
    setError(null)
    try {
      const updated = await setLibraryToolCredential(item.id, token.trim())
      clear()
      onSaved(updated)
    } catch (err) {
      clear()
      setError(credentialErrorText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title={item?.tool?.credential.set ? 'Replace credential' : 'Set credential'}
      description={item ? `The bearer token ${item.name} sends to its server.` : undefined}
      dismissible={!busy}
    >
      <form className={styles.body} onSubmit={(e) => void save(e)}>
        <Input
          ref={ref}
          label="Token"
          type="password"
          autoComplete="off"
          spellCheck={false}
          className={styles.mono}
          disabled={busy}
          error={error ?? undefined}
          aria-invalid={error ? true : undefined}
        />
        <p className={styles.text}>
          It is stored in the cluster, not in the database, and is never shown again. darkplum
          only keeps its last four characters, so you can tell which one is set.
        </p>
        <div className={styles.actions}>
          <Button type="button" variant="secondary" onClick={close} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save credential'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}
