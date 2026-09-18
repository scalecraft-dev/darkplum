import { Link } from 'react-router-dom'
import type { AdminModel, PolicyPrincipal } from '../types'
import { POLICY_RULE, modelLabel, storedSummary } from '../lib/policy'
import { Badge, Button, Table, TableEmpty } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './AccessOverviewSection.module.css'

interface Props {
  principals: PolicyPrincipal[]
  total: number
  models: AdminModel[]
  loading: boolean
  error: string | null
  onRetry: () => void
}

/** Where a principal's row is edited. The drawer lives on Users & Keys, so this
 *  links there and names which row to open rather than growing a second editor. */
function editHref(p: PolicyPrincipal): string {
  const id = p.principal_type === 'user' ? p.user_id : p.api_key_id
  const tab = p.principal_type === 'user' ? 'users' : 'api-keys'
  return `/admin/users?tab=${tab}&policy=${encodeURIComponent(id ?? '')}`
}

function ReachableCell({ ids, models }: { ids: string[]; models: AdminModel[] }) {
  if (ids.length === 0) {
    return <span className={styles.nothing}>Nothing reachable</span>
  }
  return (
    <span className={styles.chips}>
      {ids.slice(0, 4).map((id) => (
        <code key={id} className={styles.chip} title={id}>{modelLabel(id, models)}</code>
      ))}
      {ids.length > 4 && (
        <span className={styles.chipMore} title={ids.slice(4).join('\n')}>
          and {ids.length - 4} more
        </span>
      )}
    </span>
  )
}

/**
 * Every user and key that has been narrowed, with what it can actually reach.
 * Principals that inherit are not listed: they are the organisation, and the
 * card above already says what that is.
 */
export function AccessOverviewSection({ principals, total, models, loading, error, onRetry }: Props) {
  return (
    <section className={styles.section} aria-labelledby="access-overview-heading">
      <div className={styles.head}>
        <div>
          <h2 id="access-overview-heading" className={styles.title}>Access overview</h2>
          <p className={styles.subtitle}>
            Users and keys with a routing policy of their own. Everyone else inherits the
            organisation's.
          </p>
        </div>
        {!loading && !error && principals.length > 0 && (
          <span className={styles.count}>
            {principals.length}
            {total > principals.length ? ` of ${total}` : ''} narrowed
          </span>
        )}
      </div>

      <div className={styles.tableCard}>
        {error ? (
          <div className={styles.error} role="alert">
            <p className={styles.errorTitle}>Access overview could not be loaded</p>
            <p className={styles.errorDetail}>{error}</p>
            <Button variant="secondary" size="sm" onClick={onRetry}>Try again</Button>
          </div>
        ) : (
          <Table>
            <thead>
              <tr>
                <th>Principal</th>
                <th>Its own policy</th>
                <th>Reachable now</th>
                <th className={styles.actionsCol}></th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                Array.from({ length: 3 }).map((_, i) => (
                  <tr key={i}>
                    <td><Skeleton width="60%" height="14px" /></td>
                    <td><Skeleton width="70%" height="14px" /></td>
                    <td><Skeleton width="80%" height="14px" /></td>
                    <td><Skeleton width="40px" height="14px" /></td>
                  </tr>
                ))
              ) : principals.length === 0 ? (
                <TableEmpty colSpan={4}>
                  <div className={styles.empty}>
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
                    </svg>
                    <span className={styles.emptyTitle}>Everyone inherits the organisation</span>
                    <span className={styles.emptyHint}>
                      {POLICY_RULE} Narrow a single user or key from the shield button on its row in
                      Users &amp; Keys.
                    </span>
                    <Link className={styles.emptyLink} to="/admin/users">Open Users &amp; Keys</Link>
                  </div>
                </TableEmpty>
              ) : (
                principals.map((p) => {
                  const id = p.principal_type === 'user' ? p.user_id : p.api_key_id
                  return (
                    <tr key={`${p.principal_type}:${id}`}>
                      <td>
                        <div className={styles.principalCell}>
                          <Badge variant={p.principal_type === 'user' ? 'info' : 'neutral'} bordered>
                            {p.principal_type === 'user' ? 'User' : 'API key'}
                          </Badge>
                          <span className={styles.principalName}>{p.display}</span>
                          {p.owner && <span className={styles.owner}>owned by {p.owner}</span>}
                        </div>
                      </td>
                      <td className={styles.stored}>{storedSummary(p.policy, models)}</td>
                      <td>
                        <ReachableCell ids={p.reachable_models} models={models} />
                      </td>
                      <td className={styles.actionsCol}>
                        <Link className={styles.editLink} to={editHref(p)}>
                          Edit
                        </Link>
                      </td>
                    </tr>
                  )
                })
              )}
            </tbody>
          </Table>
        )}
      </div>

      {total > principals.length && (
        <p className={styles.truncated}>
          Showing the first {principals.length} of {total}. Narrow the list from Users &amp; Keys.
        </p>
      )}
    </section>
  )
}
