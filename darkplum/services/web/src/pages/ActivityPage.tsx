import { ActivityTab } from './ActivityTab'
import styles from './PageLayout.module.css'

export function ActivityPage() {
  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <h1 className={styles.title}>Activity</h1>
        <p className={styles.subtitle}>Usage analytics, audit events, and request history</p>
      </div>
      <ActivityTab />
    </div>
  )
}
