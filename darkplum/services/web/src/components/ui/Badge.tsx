import styles from './Badge.module.css'

interface BadgeProps {
  variant: 'success' | 'danger' | 'warning' | 'info' | 'neutral'
  bordered?: boolean
  children: React.ReactNode
}

export function Badge({ variant, bordered = false, children }: BadgeProps) {
  return (
    <span
      className={[
        styles.badge,
        styles[variant],
        bordered ? styles.bordered : '',
      ]
        .filter(Boolean)
        .join(' ')}
    >
      {children}
    </span>
  )
}
