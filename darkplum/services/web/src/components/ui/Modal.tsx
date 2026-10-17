import * as Dialog from '@radix-ui/react-dialog'
import styles from './Modal.module.css'

interface ModalProps {
  open: boolean
  onClose: () => void
  title?: string
  description?: string
  /**
   * False disarms the two ways of leaving a modal by accident — Escape, and a
   * click on the overlay — without removing the deliberate ones. Set it while
   * there is something in the dialog that closing would throw away, or a
   * request in flight whose outcome the dialog is the only place to read.
   *
   * It does not make the dialog inescapable: the close button and whatever
   * cancel the body offers still work, and every attempt is reported through
   * onDismissAttempt so the caller can ask rather than simply refuse. A modal
   * that swallows Escape and says nothing is a trap.
   */
  dismissible?: boolean
  /** Called instead of onClose when a dismissal is attempted on a modal that
   *  is not dismissible. */
  onDismissAttempt?: () => void
  children: React.ReactNode
}

export function Modal({
  open,
  onClose,
  title,
  description,
  dismissible = true,
  onDismissAttempt,
  children,
}: ModalProps) {
  const refuse = (e: { preventDefault: () => void }) => {
    if (dismissible) return
    e.preventDefault()
    onDismissAttempt?.()
  }

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
          onEscapeKeyDown={refuse}
          onPointerDownOutside={refuse}
          onInteractOutside={refuse}
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
          {/* Not Dialog.Close: on a modal that is not dismissible this asks
              the caller first, the same as Escape does, rather than being the
              one exit that skips the question. */}
          <button
            type="button"
            className={styles.close}
            aria-label="Close"
            onClick={() => (dismissible ? onClose() : onDismissAttempt?.())}
          >
            <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
              <line x1="3" y1="3" x2="11" y2="11" />
              <line x1="11" y1="3" x2="3" y2="11" />
            </svg>
          </button>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
