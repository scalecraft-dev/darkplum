import { useState } from 'react'
import type { User } from '../types'
import { useToast } from '../components/Toast'
import { Avatar, Badge, Button, Input, Modal, Table, TableEmpty } from '../components/ui'
import styles from './UsersTab.module.css'

interface Props {
  users: User[]
  isAdmin: boolean
  currentUserId: string | undefined
  onCreate: (name: string, email: string, password?: string) => Promise<void>
  onDelete: (id: string) => Promise<void>
  onRoleChange: (id: string, role: 'admin' | 'reader') => Promise<void>
  onResetPassword?: (id: string, password: string) => Promise<void>
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

/** Deleting a user takes their chat history with it — the database cascades
 *  conversations off the user row. That is not something a two-click inline
 *  confirm can say, so it is said in full before anything happens. */
function DeleteButton({ user, onDelete }: { user: User; onDelete: (id: string) => Promise<void> }) {
  const { toast } = useToast()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)

  const handleDelete = async () => {
    setLoading(true)
    try {
      await onDelete(user.id)
      toast('User deleted', 'success')
      setOpen(false)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <button
        className={styles.actionBtn}
        onClick={() => setOpen(true)}
        disabled={loading}
        title="Delete user"
      >
        {loading ? (
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" className={styles.spinner}>
            <path d="M21 12a9 9 0 1 1-6.219-8.56" />
          </svg>
        ) : (
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="3 6 5 6 21 6" />
            <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
          </svg>
        )}
      </button>

      <Modal
        open={open}
        onClose={() => setOpen(false)}
        title="Delete this user?"
        description={`${user.name} (${user.email}) loses access immediately, and every conversation they have had is permanently deleted with the account. There is no undo.`}
      >
        <div className={styles.formFooter}>
          <Button variant="ghost" onClick={() => setOpen(false)} disabled={loading}>
            Cancel
          </Button>
          <Button variant="danger" onClick={() => void handleDelete()} disabled={loading}>
            {loading ? 'Deleting...' : 'Delete user and conversations'}
          </Button>
        </div>
      </Modal>
    </>
  )
}

function RoleSelector({ user, onRoleChange }: { user: User; onRoleChange: (id: string, role: 'admin' | 'reader') => Promise<void> }) {
  const { toast } = useToast()
  const [loading, setLoading] = useState(false)

  const handleChange = async (e: React.ChangeEvent<HTMLSelectElement>) => {
    const role = e.target.value as 'admin' | 'reader'
    setLoading(true)
    try {
      await onRoleChange(user.id, role)
      toast('Role updated', 'success')
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }

  return (
    <select
      className={styles.roleSelect}
      value={user.role}
      onChange={handleChange}
      disabled={loading}
    >
      <option value="admin">Admin</option>
      <option value="reader">Reader</option>
    </select>
  )
}

function ResetPasswordButton({ userId, onReset }: { userId: string; onReset: (id: string, password: string) => Promise<void> }) {
  const { toast } = useToast()
  const [open, setOpen] = useState(false)
  const [pw, setPw] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (pw.length < 8) { toast('Password must be at least 8 characters', 'error'); return }
    setLoading(true)
    try {
      await onReset(userId, pw)
      toast('Password reset', 'success')
      setPw('')
      setOpen(false)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }

  if (!open) {
    return (
      <button className={styles.actionBtn} onClick={() => setOpen(true)} title="Reset password">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
          <path d="M7 11V7a5 5 0 0 1 10 0v4" />
        </svg>
      </button>
    )
  }

  return (
    <form onSubmit={handleSubmit} className={styles.resetPasswordForm}>
      <input
        type="password"
        value={pw}
        onChange={e => setPw(e.target.value)}
        placeholder="New password"
        minLength={8}
        required
        autoFocus
        className={styles.resetPasswordInput}
      />
      <button type="submit" className={styles.actionBtn} disabled={loading} title="Confirm">
        {loading ? '...' : 'Set'}
      </button>
      <button type="button" className={styles.actionBtn} onClick={() => { setOpen(false); setPw('') }} title="Cancel">
        X
      </button>
    </form>
  )
}

export function UsersTab({ users, isAdmin, currentUserId, onCreate, onDelete, onRoleChange, onResetPassword }: Props) {
  const { toast } = useToast()
  const [modalOpen, setModalOpen] = useState(false)
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [emailError, setEmailError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!EMAIL_RE.test(email)) {
      setEmailError('Enter a valid email address')
      return
    }
    setEmailError('')
    setLoading(true)
    try {
      await onCreate(name.trim(), email.trim(), password)
      toast('User created', 'success')
      setName('')
      setEmail('')
      setPassword('')
      setModalOpen(false)
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setLoading(false)
    }
  }

  const handleClose = () => {
    setModalOpen(false)
    setName('')
    setEmail('')
    setPassword('')
    setEmailError('')
  }

  return (
    <div>
      <div className={styles.toolbar}>
        <div className={styles.toolbarInfo}>
          <span className={styles.count}>{users.length} {users.length === 1 ? 'user' : 'users'}</span>
        </div>
        {isAdmin && (
          <Button size="sm" onClick={() => setModalOpen(true)}>
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <line x1="12" y1="5" x2="12" y2="19" />
              <line x1="5" y1="12" x2="19" y2="12" />
            </svg>
            Add User
          </Button>
        )}
      </div>

      <div className={styles.tableCard}>
        <Table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Email</th>
              <th>Role</th>
              <th>Created</th>
              <th className={styles.actionsCol}></th>
            </tr>
          </thead>
          <tbody>
            {users.length === 0 ? (
              <TableEmpty colSpan={5}>
                <div className={styles.empty}>
                  <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
                    <circle cx="9" cy="7" r="4" />
                    <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
                    <path d="M16 3.13a4 4 0 0 1 0 7.75" />
                  </svg>
                  <span className={styles.emptyTitle}>No users yet</span>
                  <span className={styles.emptyHint}>Add a user to get started</span>
                </div>
              </TableEmpty>
            ) : (
              users.map((u) => (
                <tr key={u.id}>
                  <td>
                    <div className={styles.userCell}>
                      <Avatar name={u.name} size="sm" />
                      <span className={styles.userName}>{u.name}</span>
                      <span className={u.auth_source === 'sso' ? styles.badgeSso : styles.badgeManual}>
                        {u.auth_source === 'sso' ? 'SSO' : 'Manual'}
                      </span>
                    </div>
                  </td>
                  <td className={styles.muted}>{u.email}</td>
                  <td>
                    {isAdmin && u.id !== currentUserId ? (
                      <RoleSelector user={u} onRoleChange={onRoleChange} />
                    ) : (
                      <Badge variant={u.role === 'admin' ? 'info' : 'neutral'}>
                        {u.role === 'admin' ? 'Admin' : 'Reader'}
                      </Badge>
                    )}
                  </td>
                  <td className={styles.date}>{new Date(u.created_at).toLocaleDateString()}</td>
                  <td className={styles.actionsCol}>
                    {isAdmin && onResetPassword && (
                      <ResetPasswordButton userId={u.id} onReset={onResetPassword} />
                    )}
                    {isAdmin && <DeleteButton user={u} onDelete={onDelete} />}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </Table>
      </div>

      <Modal open={modalOpen} onClose={handleClose} title="Add User" description="Create a new user for this deployment.">
        <form onSubmit={handleSubmit} className={styles.form}>
          <Input
            label="Name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Full name"
            required
            autoFocus
          />
          <Input
            label="Email"
            type="text"
            value={email}
            onChange={(e) => { setEmail(e.target.value); setEmailError('') }}
            placeholder="user@example.com"
            error={emailError}
            required
          />
          <Input
            label="Password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="At least 8 characters"
            minLength={8}
            required
          />
          <div className={styles.formFooter}>
            <Button type="button" variant="ghost" onClick={handleClose}>Cancel</Button>
            <Button type="submit" disabled={loading || !name.trim()}>
              {loading ? 'Creating...' : 'Create User'}
            </Button>
          </div>
        </form>
      </Modal>
    </div>
  )
}
