import { useCallback, useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  createApiKey,
  createUser,
  deleteUser,
  listApiKeys,
  listAdminModels,
  listModelAliases,
  listPolicyPrincipals,
  listUsers,
  resetUserPassword,
  revokeApiKey,
  updateUserRole,
} from '../lib/api'
import type { AdminModel, ApiKey, ModelAlias, PolicyPrincipal, User } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { Tab, TabList } from '../components/ui'
import { KeyRevealModal } from './KeyRevealModal'
import { UsersTab } from './UsersTab'
import { ApiKeysTab } from './ApiKeysTab'
import { PrincipalPolicyDrawer, type PolicyPrincipalRef } from './PrincipalPolicyDrawer'
import pageStyles from './PageLayout.module.css'
import styles from './AdminPage.module.css'

type Tab = 'users' | 'api-keys'

export function AdminPage() {
  const { user: currentUser } = useAuth()
  const isAdmin = currentUser?.role === 'admin'
  const { toast } = useToast()
  const [searchParams, setSearchParams] = useSearchParams()
  const [tab, setTab] = useState<Tab>(() =>
    searchParams.get('tab') === 'api-keys' ? 'api-keys' : 'users',
  )
  const [users, setUsers] = useState<User[]>([])
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [revealKey, setRevealKey] = useState<string | null>(null)

  // The policy drawer needs the model catalogue and the aliases; the lists need
  // to know which principals have a row of their own. All three are read once
  // here rather than by each tab.
  const [models, setModels] = useState<AdminModel[]>([])
  const [aliases, setAliases] = useState<ModelAlias[]>([])
  const [principals, setPrincipals] = useState<PolicyPrincipal[]>([])
  const [policyTarget, setPolicyTarget] = useState<PolicyPrincipalRef | null>(null)

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

  /** Which users and keys carry their own policy. A failure here costs a badge,
   *  not the page, so it is not worth a toast. */
  const loadPrincipals = useCallback(async () => {
    try {
      setPrincipals((await listPolicyPrincipals({ limit: 200 })).principals)
    } catch {
      setPrincipals([])
    }
  }, [])

  useEffect(() => {
    void loadUsers()
    void loadKeys()
    void loadPrincipals()
    // The catalogue only matters once the drawer opens, and a failure there is
    // reported inside the drawer.
    listAdminModels().then(setModels).catch(() => setModels([]))
    listModelAliases().then(setAliases).catch(() => setAliases([]))
  }, [loadUsers, loadKeys, loadPrincipals])

  const policyOwners = useMemo(() => {
    const ids = new Set<string>()
    for (const p of principals) {
      const id = p.principal_type === 'user' ? p.user_id : p.api_key_id
      if (id) ids.add(id)
    }
    return ids
  }, [principals])

  const openUserPolicy = useCallback((user: User) => {
    setPolicyTarget({
      type: 'user',
      id: user.id,
      title: user.name || user.email,
      subtitle: user.email,
    })
  }, [])

  const openKeyPolicy = useCallback((key: ApiKey) => {
    setPolicyTarget({
      type: 'api_key',
      id: key.id,
      title: key.name,
      subtitle: `${key.prefix}… · owned by ${key.user_name ?? 'unknown user'}`,
    })
  }, [])

  // Deep link from the access overview on the Models page: ?tab=…&policy=<id>.
  // The drawer opens as soon as the row it names has been read, and the query
  // is cleared so a reload does not reopen it unasked.
  useEffect(() => {
    const target = searchParams.get('policy')
    if (!target) return
    const key = keys.find((k) => k.id === target)
    const user = users.find((u) => u.id === target)
    if (!key && !user) return
    if (key) {
      setTab('api-keys')
      openKeyPolicy(key)
    } else if (user) {
      setTab('users')
      openUserPolicy(user)
    }
    const next = new URLSearchParams(searchParams)
    next.delete('policy')
    setSearchParams(next, { replace: true })
  }, [searchParams, setSearchParams, users, keys, openUserPolicy, openKeyPolicy])

  const handleCreateUser = async (
    name: string,
    email: string,
    password: string,
    role: 'admin' | 'reader',
  ) => {
    const user = await createUser({ name, email, password, role })
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
        <p className={pageStyles.subtitle}>Manage users, API keys and what each one can reach</p>
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
          <UsersTab
            users={users}
            isAdmin={isAdmin}
            currentUserId={currentUser?.id}
            onCreate={handleCreateUser}
            onDelete={handleDeleteUser}
            onRoleChange={handleRoleChange}
            onResetPassword={handleResetPassword}
            onEditPolicy={openUserPolicy}
            policyOwners={policyOwners}
          />
        )}
        {tab === 'api-keys' && (
          <ApiKeysTab
            users={users}
            apiKeys={keys}
            isAdmin={isAdmin}
            onCreate={handleCreateKey}
            onRevoke={handleRevoke}
            onEditPolicy={openKeyPolicy}
            policyOwners={policyOwners}
          />
        )}
      </div>

      {revealKey && (
        <KeyRevealModal keyValue={revealKey} onClose={() => setRevealKey(null)} />
      )}

      <PrincipalPolicyDrawer
        principal={policyTarget}
        models={models}
        aliases={aliases}
        onClose={() => setPolicyTarget(null)}
        onSaved={() => void loadPrincipals()}
      />
    </div>
  )
}
