import { createContext, useCallback, useContext, useRef, useState } from 'react'
import styles from './Toast.module.css'

type ToastKind = 'success' | 'error' | 'warning' | 'info'

interface Toast {
  id: number
  message: string
  kind: ToastKind
  exiting: boolean
}

interface ToastContextValue {
  toast: (message: string, kind: ToastKind) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

let _nextId = 0

const EXIT_DURATION = 220 // ms — slightly longer than the 0.2s CSS transition

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const timers = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map())

  const remove = useCallback((id: number) => {
    // Trigger exit animation, then actually unmount after the transition completes
    setToasts((prev) =>
      prev.map((t) => (t.id === id ? { ...t, exiting: true } : t)),
    )
    const exitTimer = setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id))
      timers.current.delete(id)
    }, EXIT_DURATION)
    timers.current.set(id, exitTimer)
  }, [])

  const toast = useCallback(
    (message: string, kind: ToastKind) => {
      const id = ++_nextId
      setToasts((prev) => [...prev, { id, message, kind, exiting: false }])
      const timer = setTimeout(() => remove(id), 4000)
      timers.current.set(id, timer)
    },
    [remove],
  )

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className={styles.container} aria-live="polite">
        {toasts.map((t) => (
          <div
            key={t.id}
            className={`${styles.toast} ${styles[t.kind]}${t.exiting ? ` ${styles.exiting}` : ''}`}
          >
            {t.message}
            <button
              className={styles.dismiss}
              onClick={() => {
                clearTimeout(timers.current.get(t.id))
                remove(t.id)
              }}
              aria-label="Dismiss"
            >
              <svg
                className={styles.dismissIcon}
                viewBox="0 0 14 14"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
              >
                <line x1="2" y1="2" x2="12" y2="12" />
                <line x1="12" y1="2" x2="2" y2="12" />
              </svg>
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used within ToastProvider')
  return ctx
}
