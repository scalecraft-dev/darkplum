import { useCallback, useEffect, useState } from 'react'
import { createApiKey, createUser, deleteUser, listApiKeys, listUsers, resetUserPassword, revokeApiKey, updateUserRole } from '../lib/api'
import type { ApiKey, User } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { Tab, TabList } from '../components/ui'
import { KeyRevealModal } from './KeyRevealModal'
import { UsersTab } from './UsersTab'
import { ApiKeysTab } from './ApiKeysTab'
import pageStyles from './PageLayout.module.css'
import styles from './AdminPage.module.css'

type Tab = 'users' | 'api-keys'

export function AdminPage() {
  const { user: currentUser } = useAuth()
  const isAdmin = currentUser?.role === 'admin'
  const { toast } = useToast()
  const [tab, setTab] = useState<Tab>('users')
  const [users, setUsers] = useState<User[]>([])
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [revealKey, setRevealKey] = useState<string | null>(null)

  const loadUsers = useCallback(async () => {
    try {
      setUsers(await listUsers())
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }, [toast])

  const loadKeys = useCallback(async () => {
    try {
      setKeys(await listApiKeys())
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }, [toast])

  useEffect(() => {
    void loadUsers()
    void loadKeys()
  }, [loadUsers, loadKeys])

  const handleCreateUser = async (name: string, email: string, password?: string) => {
    const user = await createUser({ name, email, password })
    setUsers((prev) => [user, ...prev])
  }

  const handleDeleteUser = async (id: string) => {
    await deleteUser(id)
    setUsers((prev) => prev.filter((u) => u.id !== id))
  }

  const handleCreateKey = async (name: string, userId: string) => {
    const key = await createApiKey({ name, user_id: userId })
    setKeys((prev) => [key, ...prev])
    if (key.key) setRevealKey(key.key)
  }

  const handleResetPassword = async (id: string, password: string) => {
    await resetUserPassword(id, password)
  }

  const handleRoleChange = async (id: string, role: 'admin' | 'reader') => {
    const res = await updateUserRole(id, role)
    const newRole = res.role as 'admin' | 'reader'
    setUsers((prev) =>
      prev.map((u) => (u.id === id ? { ...u, role: newRole } : u)),
    )
  }

  const handleRevoke = async (id: string) => {
    await revokeApiKey(id)
    setKeys((prev) =>
      prev.map((k) => (k.id === id ? { ...k, revoked_at: new Date().toISOString() } : k)),
    )
  }

  return (
    <div className={pageStyles.page}>
      <div className={pageStyles.header}>
        <h1 className={pageStyles.title}>Users & Keys</h1>
        <p className={pageStyles.subtitle}>Manage users and API keys</p>
      </div>

      <TabList>
        <Tab
          active={tab === 'users'}
          count={users.length > 0 ? users.length : undefined}
          onClick={() => setTab('users')}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
            <circle cx="9" cy="7" r="4" />
            <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
            <path d="M16 3.13a4 4 0 0 1 0 7.75" />
          </svg>
          Users
        </Tab>
        <Tab
          active={tab === 'api-keys'}
          count={keys.length > 0 ? keys.length : undefined}
          onClick={() => setTab('api-keys')}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
          </svg>
          API Keys
        </Tab>
      </TabList>

      <div className={styles.tabContent}>
        {tab === 'users' && (
          <UsersTab users={users} isAdmin={isAdmin} currentUserId={currentUser?.id} onCreate={handleCreateUser} onDelete={handleDeleteUser} onRoleChange={handleRoleChange} onResetPassword={handleResetPassword} />
        )}
        {tab === 'api-keys' && (
          <ApiKeysTab users={users} apiKeys={keys} isAdmin={isAdmin} onCreate={handleCreateKey} onRevoke={handleRevoke} />
        )}
      </div>

      {revealKey && (
        <KeyRevealModal keyValue={revealKey} onClose={() => setRevealKey(null)} />
      )}
    </div>
  )
}
