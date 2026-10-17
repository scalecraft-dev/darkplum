import styles from './Badge.module.css'

interface BadgeProps {
  variant: 'success' | 'danger' | 'warning' | 'info' | 'neutral'
  bordered?: boolean
  /**
   * A dashed border instead of a solid one: this state is provisional, not
   * settled. Used for what no backend has reported yet — an unknown class, an
   * alias target whose home is not known — so "we do not know" is visibly a
   * different kind of thing from "we know, and it is bad". Implies `bordered`.
   */
  dashed?: boolean
  /** Hover and accessible text for a badge whose one or two words need the
   *  rest of the sentence — the External API class, above all. */
  title?: string
  children: React.ReactNode
}

export function Badge({ variant, bordered = false, dashed = false, title, children }: BadgeProps) {
  return (
    <span
      className={[
        styles.badge,
        styles[variant],
        bordered || dashed ? styles.bordered : '',
        dashed ? styles.dashed : '',
      ]
        .filter(Boolean)
        .join(' ')}
      title={title}
    >
      {children}
    </span>
  )
}
