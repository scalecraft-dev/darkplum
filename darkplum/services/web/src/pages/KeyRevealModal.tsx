import { Button, CopyButton, Modal } from '../components/ui'
import styles from './KeyRevealModal.module.css'

interface Props {
  keyValue: string
  onClose: () => void
}

export function KeyRevealModal({ keyValue, onClose }: Props) {
  return (
    <Modal open onClose={onClose} title="API Key Created">
      <p className={styles.warning}>
        Copy this key now. It will not be shown again.
      </p>
      <div className={styles.keyBlock}>
        <code className={styles.keyText}>{keyValue}</code>
        <CopyButton value={keyValue} />
      </div>
      <div className={styles.footer}>
        <Button onClick={onClose}>Done</Button>
      </div>
    </Modal>
  )
}
