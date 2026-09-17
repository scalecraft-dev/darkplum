import styles from './Table.module.css'

interface TableProps extends React.TableHTMLAttributes<HTMLTableElement> {
  children: React.ReactNode
}

export function Table({ children, className, ...props }: TableProps) {
  return (
    <table className={`${styles.table} ${className ?? ''}`} {...props}>
      {children}
    </table>
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
