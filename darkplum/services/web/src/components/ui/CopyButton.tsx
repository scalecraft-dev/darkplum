import { useState } from 'react'
import styles from './CopyButton.module.css'

interface CopyButtonProps {
  value: string
  label?: string
  className?: string
  onError?: () => void
}

export function CopyButton({ value, label = 'Copy', className, onError }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setFailed(false)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setFailed(true)
      setTimeout(() => setFailed(false), 2000)
      onError?.()
    }
  }

  return (
    <button
      className={`${styles.btn} ${failed ? styles.failed : ''} ${className ?? ''}`}
      onClick={handleCopy}
      type="button"
    >
      {copied ? (
        <svg className={styles.icon} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <polyline points="3.5 8.5 6.5 11.5 12.5 5.5" />
        </svg>
      ) : failed ? null : (
        <svg className={styles.icon} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect x="5" y="5" width="8" height="8" rx="1.5" />
          <path d="M3 11V3h8" />
        </svg>
      )}
      <span aria-live="polite">
        {copied ? 'Copied' : failed ? 'Select & copy manually' : label}
      </span>
    </button>
  )
}
