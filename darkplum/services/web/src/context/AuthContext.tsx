import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import type { AuthConfig } from '../types'
import { ROUTES } from '../lib/routes'

interface AuthUser {
  id: string
  email: string
  name: string
  role: 'admin' | 'reader'
  picture?: string | null
  has_password?: boolean
}

interface AuthContextValue {
  user: AuthUser | null
  authConfig: AuthConfig | null
  loading: boolean
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  setup: (name: string, email: string, password: string) => Promise<void>
}

const AuthContext = createContext<AuthContextValue>({
  user: null,
  authConfig: null,
  loading: true,
  login: async () => {},
  logout: async () => {},
  setup: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [authConfig, setAuthConfig] = useState<AuthConfig | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    Promise.all([
      fetch(ROUTES.authConfig.path, { credentials: 'same-origin' })
        .then(res => res.ok ? res.json() as Promise<AuthConfig> : null)
        .catch(() => null),
      fetch(ROUTES.authMe.path, { credentials: 'same-origin' })
        .then(res => res.ok ? res.json() as Promise<AuthUser> : null)
        .catch(() => null),
    ]).then(([config, me]) => {
      setAuthConfig(config)
      setUser(me)
    }).finally(() => setLoading(false))
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    const res = await fetch(ROUTES.authLogin.path, {
      method: ROUTES.authLogin.method,
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
      credentials: 'same-origin',
      body: JSON.stringify({ email, password }),
    })
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: 'Login failed' }))
      throw new Error((body as { error?: string }).error || 'Login failed')
    }
    const data = await res.json() as AuthUser
    setUser(data)
    setAuthConfig(prev => prev ? { ...prev, setup_required: false } : prev)
  }, [])

  const setup = useCallback(async (name: string, email: string, password: string) => {
    const res = await fetch(ROUTES.authSetup.path, {
      method: ROUTES.authSetup.method,
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
      credentials: 'same-origin',
      body: JSON.stringify({ name, email, password }),
    })
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: 'Setup failed' }))
      throw new Error((body as { error?: string }).error || 'Setup failed')
    }
    const data = await res.json() as AuthUser
    setUser(data)
    setAuthConfig(prev => prev ? { ...prev, setup_required: false } : prev)
  }, [])

  const logout = useCallback(async () => {
    try {
      const res = await fetch(ROUTES.authLogout.path, {
        method: ROUTES.authLogout.method,
        headers: { 'X-Requested-With': 'XMLHttpRequest' },
        credentials: 'same-origin',
      })
      if (res.ok) {
        const data = await res.json() as { redirect?: string }
        setUser(null)
        if (data.redirect && data.redirect.startsWith('http')) {
          window.location.href = data.redirect
          return
        }
      }
    } catch {
      // Fall through
    }
    setUser(null)
  }, [])

  return (
    <AuthContext.Provider value={{ user, authConfig, loading, login, logout, setup }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
