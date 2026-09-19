import { useCallback, useEffect, useRef, useState } from 'react'
import styles from './Table.module.css'

interface TableProps extends React.TableHTMLAttributes<HTMLTableElement> {
  /** What this table is, for the scroll region it sits in. Only announced when
   *  the table is actually too wide to fit, which is when someone needs to be
   *  told there is more of it off to the side. */
  label: string
  children: React.ReactNode
}

/**
 * A table that keeps its columns rather than collapsing them.
 *
 * Below the table's minimum width the wrapper scrolls sideways instead of
 * letting the browser crush a monospace model id into one character per line.
 * A region that scrolls has to be reachable by keyboard (WCAG 2.1.1), so the
 * wrapper becomes a named, focusable region exactly while it overflows — and
 * goes back to being an unremarkable div on a laptop, where it never does.
 */
export function Table({ label, children, className, ...props }: TableProps) {
  const ref = useRef<HTMLDivElement>(null)
  const [scrollable, setScrollable] = useState(false)

  const measure = useCallback(() => {
    const el = ref.current
    if (!el) return
    // 1px of slack: sub-pixel layout rounding should not advertise a scroll
    // region that cannot actually move.
    setScrollable(el.scrollWidth - el.clientWidth > 1)
  }, [])

  useEffect(() => {
    const el = ref.current
    if (!el) return
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(el)
    // The table's own width changes when rows arrive, not just when the
    // viewport does.
    const table = el.firstElementChild
    if (table) observer.observe(table)
    return () => observer.disconnect()
  }, [measure])

  return (
    <div
      ref={ref}
      className={styles.scroll}
      {...(scrollable
        ? { role: 'region', 'aria-label': label, tabIndex: 0 }
        : {})}
    >
      <table className={`${styles.table} ${className ?? ''}`} {...props}>
        {children}
      </table>
    </div>
  )
}

export function TableEmpty({ colSpan, children }: { colSpan: number; children: React.ReactNode }) {
  return (
    <tr>
      <td colSpan={colSpan} className={styles.empty}>
        {children}
      </td>
    </tr>
  )
}
