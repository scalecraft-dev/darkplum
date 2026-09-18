import { useEffect, useMemo, useRef, useState } from 'react'
import type { AdminModel, ApiKey, PrincipalPolicy, User } from '../types'
import { storedSummary } from '../lib/policy'
import { useToast } from '../components/Toast'
import { Badge, Button, Input, Modal, Select, Table, TableEmpty } from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import styles from './ApiKeysTab.module.css'

interface Props {
  users: User[]
  apiKeys: ApiKey[]
  /** For labelling model ids in the "Model access" cell's summary. */
  models: AdminModel[]
  isAdmin: boolean
  onCreate: (name: string, userId: string) => Promise<void>
  onRevoke: (id: string) => Promise<void>
  /** Opens the routing-policy drawer for one key. */
  onEditPolicy?: (key: ApiKey) => void
  /** Each key's own routing-policy row, by id. Absent means it inherits its
   *  owner's — the "Model access" cell reads this either way. */
  policyById?: ReadonlyMap<string, PrincipalPolicy>
}

function RevokeButton({ id, onRevoke }: { id: string; onRevoke: (id: string) => Promise<void> }) {
  const { toast } = useToast()
  const [confirming, setConfirming] = useState(false)
  const [loading, setLoading] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const handleClick = async () => {
    if (!confirming) {
      setConfirming(true)
      timer.current = setTimeout(() => setConfirming(false), 3000)
      return
    }
    if (timer.current) clearTimeout(timer.current)
    setLoading(true)
    try {
      await onRevoke(id)
      toast('Key revoked', 'success')
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
      setConfirming(false)
    }
  }

  useEffect(() => () => { if (timer.current) clearTimeout(timer.current) }, [])

  return (
    <button
      className={`${styles.actionBtn} ${confirming ? styles.actionConfirm : ''}`}
      onClick={handleClick}
      disabled={loading}
      title="Revoke key"
    >
      {loading ? (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" className={styles.spinner}>
          <path d="M21 12a9 9 0 1 1-6.219-8.56" />
        </svg>
      ) : confirming ? (
        'Confirm?'
      ) : (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <polyline points="3 6 5 6 21 6" />
          <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
        </svg>
      )}
    </button>
  )
}

/** The "Model access" cell itself: what the key's policy actually is, and the
 *  way into changing it — a key never reaches more than its owner, so this
 *  narrows; it cannot widen. Falls back to plain text when there's nothing to
 *  click into. */
function PolicyCell({
  policy,
  models,
  label,
  onClick,
}: {
  policy: PrincipalPolicy | undefined
  models: AdminModel[]
  label: string
  onClick?: () => void
}) {
  const summary = policy ? storedSummary(policy, models) : 'Inherits owner'
  if (!onClick) {
    return <span className={styles.muted}>{summary}</span>
  }
  return (
    <button type="button" className={styles.policyCell} onClick={onClick} title={label} aria-label={label}>
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
      </svg>
      <span>{summary} · {policy ? 'edit' : 'narrow'}</span>
    </button>
  )
}

export function ApiKeysTab({ users, apiKeys, models, isAdmin, onCreate, onRevoke, onEditPolicy, policyById }: Props) {
  const { toast } = useToast()
  const [modalOpen, setModalOpen] = useState(false)
  const [keyName, setKeyName] = useState('')
  const [userId, setUserId] = useState('')
  const [loading, setLoading] = useState(false)
  const [query, setQuery] = useState('')

  const activeCount = apiKeys.filter((k) => !k.revoked_at).length
  const revokedCount = apiKeys.filter((k) => k.revoked_at).length

  const filteredKeys = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return apiKeys
    return apiKeys.filter(
      (k) => k.name.toLowerCase().includes(q) || (k.user_name ?? '').toLowerCase().includes(q),
    )
  }, [apiKeys, query])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    try {
      await onCreate(keyName.trim(), userId)
      setKeyName('')
      setUserId('')
      setModalOpen(false)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }

  const handleClose = () => {
    setModalOpen(false)
    setKeyName('')
    setUserId('')
  }

  return (
    <div>
      <div className={styles.toolbar}>
        <div className={styles.toolbarInfo}>
          {query.trim() ? (
            <span className={styles.count}>{filteredKeys.length} of {apiKeys.length} keys</span>
          ) : (
            <>
              <span className={styles.count}>{activeCount} active</span>
              {revokedCount > 0 && (
                <span className={styles.countMuted}>{revokedCount} revoked</span>
              )}
            </>
          )}
        </div>
        <div className={styles.searchWrap}>
          <svg
            className={styles.searchIcon}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            className={styles.searchInput}
            type="text"
            placeholder="Search key name or owner..."
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search API keys by name or owner"
          />
        </div>
        {isAdmin && (
          <Button size="sm" onClick={() => setModalOpen(true)}>
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <line x1="12" y1="5" x2="12" y2="19" />
              <line x1="5" y1="12" x2="19" y2="12" />
            </svg>
            Generate Key
          </Button>
        )}
      </div>

      <div className={styles.tableCard}>
        <Table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Owner</th>
              <th>Key Prefix</th>
              <th>Created</th>
              <th>Model access</th>
              <th>Status</th>
              <th className={styles.actionsCol}></th>
            </tr>
          </thead>
          <tbody>
            {apiKeys.length === 0 ? (
              <TableEmpty colSpan={7}>
                <div className={styles.empty}>
                  <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
                  </svg>
                  <span className={styles.emptyTitle}>No API keys yet</span>
                  <span className={styles.emptyHint}>Generate a key to enable API access</span>
                </div>
              </TableEmpty>
            ) : filteredKeys.length === 0 ? (
              <TableEmpty colSpan={7}>
                <div className={styles.empty}>
                  <span className={styles.emptyTitle}>No keys match "{query}"</span>
                  <span className={styles.emptyHint}>Try a different key name or owner.</span>
                </div>
              </TableEmpty>
            ) : (
              filteredKeys.map((k) => (
                <tr key={k.id} className={k.revoked_at ? styles.revokedRow : undefined}>
                  <td>
                    <div className={styles.keyCell}>
                      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className={styles.keyIcon}>
                        <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
                      </svg>
                      <span className={styles.keyName}>{k.name}</span>
                    </div>
                  </td>
                  <td className={styles.muted}>{k.user_name ?? '—'}</td>
                  <td>
                    <div className={styles.prefixCell}>
                      <code className={styles.prefix}>{k.prefix}...</code>
                      <CopyButton value={k.prefix} />
                    </div>
                  </td>
                  <td className={styles.date}>{new Date(k.created_at).toLocaleDateString()}</td>
                  <td>
                    <PolicyCell
                      policy={policyById?.get(k.id)}
                      models={models}
                      label={`Routing policy for ${k.name}`}
                      onClick={isAdmin && !k.revoked_at && onEditPolicy ? () => onEditPolicy(k) : undefined}
                    />
                  </td>
                  <td>
                    <Badge variant={k.revoked_at ? 'danger' : 'success'}>
                      {k.revoked_at ? 'Revoked' : 'Active'}
                    </Badge>
                  </td>
                  <td className={styles.actionsCol}>
                    {isAdmin && !k.revoked_at && <RevokeButton id={k.id} onRevoke={onRevoke} />}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </Table>
      </div>

      <Modal open={modalOpen} onClose={handleClose} title="Generate API Key" description="Create a new API key for programmatic access.">
        <form onSubmit={handleSubmit} className={styles.form}>
          <Input
            label="Key Name"
            type="text"
            value={keyName}
            onChange={(e) => setKeyName(e.target.value)}
            placeholder="e.g. prod-service"
            required
            autoFocus
          />
          <Select
            label="Owner"
            value={userId}
            onChange={(e) => setUserId(e.target.value)}
            required
          >
            <option value="">Select a user</option>
            {users.map((u) => (
              <option key={u.id} value={u.id}>{u.name}</option>
            ))}
          </Select>
          <p className={styles.ownerNote}>
            The key starts with exactly what its owner can reach. Narrow it further from the Model
            access cell on its row — a key can never reach more than its owner.
          </p>
          <div className={styles.formFooter}>
            <Button type="button" variant="ghost" onClick={handleClose}>Cancel</Button>
            <Button type="submit" disabled={loading || !keyName.trim() || !userId}>
              {loading ? 'Generating...' : 'Generate Key'}
            </Button>
          </div>
        </form>
      </Modal>
    </div>
  )
}
