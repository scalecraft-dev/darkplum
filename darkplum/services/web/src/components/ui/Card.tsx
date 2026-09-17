import styles from './Card.module.css'

interface CardProps {
  children: React.ReactNode
  className?: string
}

interface CardHeaderProps {
  label: string
  action?: React.ReactNode
}

export function Card({ children, className }: CardProps) {
  return (
    <div className={`${styles.card} ${className ?? ''}`}>
      {children}
    </div>
  )
}

export function CardHeader({ label, action }: CardHeaderProps) {
  return (
    <div className={styles.header}>
      <span className={styles.label}>{label}</span>
      {action}
    </div>
  )
}
