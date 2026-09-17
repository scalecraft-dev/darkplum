import * as Dialog from '@radix-ui/react-dialog'
import styles from './Modal.module.css'

interface ModalProps {
  open: boolean
  onClose: () => void
  title?: string
  description?: string
  children: React.ReactNode
}

export function Modal({ open, onClose, title, description, children }: ModalProps) {
  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.overlay} />
        {/*
          When no description is provided, pass aria-describedby={undefined}
          so Radix omits the attribute entirely rather than pointing to a
          missing element — prevents screen readers from announcing nothing.
          When a description is provided, Dialog.Description below sets up
          the association automatically; we leave aria-describedby off here.
        */}
        <Dialog.Content
          className={styles.content}
          {...(!description && { 'aria-describedby': undefined })}
        >
          {title && (
            <Dialog.Title className={styles.title}>{title}</Dialog.Title>
          )}
          {description && (
            <Dialog.Description className={styles.description}>
              {description}
            </Dialog.Description>
          )}
          {children}
          <Dialog.Close asChild>
            <button type="button" className={styles.close} aria-label="Close">
              <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
                <line x1="3" y1="3" x2="11" y2="11" />
                <line x1="11" y1="3" x2="3" y2="11" />
              </svg>
            </button>
          </Dialog.Close>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
