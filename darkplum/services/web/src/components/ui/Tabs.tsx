import styles from './Tabs.module.css'

// ---------------------------------------------------------------------------
// TabList — the container row
// ---------------------------------------------------------------------------

interface TabListProps {
  children: React.ReactNode
}

export function TabList({ children }: TabListProps) {
  return (
    <div className={styles.tabList} role="tablist">
      {children}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Tab — a single tab button
// ---------------------------------------------------------------------------

interface TabProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  active?: boolean
  count?: number
  children: React.ReactNode
}

export function Tab({ active = false, count, children, ...props }: TabProps) {
  return (
    <button
      role="tab"
      aria-selected={active}
      className={`${styles.tab} ${active ? styles.tabActive : ''}`}
      {...props}
    >
      {children}
      {count !== undefined && (
        <span className={styles.tabCount}>{count}</span>
      )}
    </button>
  )
}
