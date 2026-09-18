import { useState, useEffect } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { useAuth } from '../context/AuthContext'
import { ROUTES, buildPath } from '../lib/routes'
import { Button, Input, PlumMark } from '../components/ui'
import styles from './LoginPage.module.css'

export function LoginPage() {
  const { authConfig, login, setup, user, loading } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const returnTo = (location.state as { returnTo?: string })?.returnTo || '/chat'

  // Already logged in — redirect
  useEffect(() => {
    if (user) {
      navigate(returnTo, { replace: true })
    }
  }, [user, navigate, returnTo])

  if (user) return null

  if (loading) {
    return (
      <div className={styles.page}>
        <div className={styles.card}>
          <div className={styles.loadingState}>
            <span className={styles.loadingMark}>
              <PlumMark size={48} />
            </span>
            <span className={styles.loadingText}>Loading...</span>
          </div>
        </div>
      </div>
    )
  }

  if (!authConfig) {
    return (
      <div className={styles.page}>
        <div className={styles.card}>
          <div className={styles.brand}>
            <PlumMark size={48} />
            <span className={styles.brandName}>darkplum</span>
            <span className={styles.brandTagline}>Private AI</span>
          </div>
          <div className={styles.error}>
            Unable to connect to the server. Please check that the service is running and try again.
          </div>
          <Button size="lg" style={{ width: '100%', marginTop: 'var(--space-2)' }} onClick={() => window.location.reload()}>
            Retry
          </Button>
        </div>
      </div>
    )
  }

  if (authConfig.setup_required) {
    return (
      <div className={styles.page}>
        <div className={styles.card}>
          <div className={styles.brand}>
            <PlumMark size={48} />
            <span className={styles.brandName}>darkplum</span>
            <span className={styles.brandTagline}>Private AI</span>
          </div>
          <p className={styles.subtitle}>Create your admin account to get started.</p>
          <SetupForm onSetup={setup} returnTo={returnTo} />
        </div>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <div className={styles.card}>
        <div className={styles.brand}>
          <PlumMark size={48} />
          <span className={styles.brandName}>darkplum</span>
          <span className={styles.brandTagline}>Private AI</span>
        </div>
        <LoginForm onLogin={login} returnTo={returnTo} />
        {authConfig.oidc?.enabled && (
          <>
            <div className={styles.divider}><span>or</span></div>
            <Button
              variant="secondary"
              size="lg"
              style={{ width: '100%' }}
              onClick={() => {
                window.location.href = buildPath(ROUTES.authLoginRedirect, undefined, {
                  return_to: returnTo,
                })
              }}
            >
              Sign in with SSO
            </Button>
          </>
        )}
      </div>
    </div>
  )
}

function LoginForm({ onLogin, returnTo }: { onLogin: (email: string, password: string) => Promise<void>; returnTo: string }) {
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      await onLogin(email, password)
      navigate(returnTo, { replace: true })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <form onSubmit={handleSubmit} className={styles.form}>
      {error && <div className={styles.error}>{error}</div>}
      <Input
        label="Email"
        type="email"
        value={email}
        onChange={e => setEmail(e.target.value)}
        required
        autoFocus
        autoComplete="email"
      />
      <Input
        label="Password"
        type="password"
        value={password}
        onChange={e => setPassword(e.target.value)}
        required
        autoComplete="current-password"
      />
      <Button type="submit" size="lg" style={{ width: '100%', marginTop: 'var(--space-2)' }} disabled={loading}>
        {loading ? 'Signing in...' : 'Sign in'}
      </Button>
    </form>
  )
}

function SetupForm({ onSetup, returnTo }: { onSetup: (name: string, email: string, password: string) => Promise<void>; returnTo: string }) {
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (password.length < 8) {
      setError('Password must be at least 8 characters')
      return
    }
    setLoading(true)
    try {
      await onSetup(name, email, password)
      navigate(returnTo, { replace: true })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <form onSubmit={handleSubmit} className={styles.form}>
      {error && <div className={styles.error}>{error}</div>}
      <Input
        label="Name"
        type="text"
        value={name}
        onChange={e => setName(e.target.value)}
        required
        autoFocus
        placeholder="Your name"
      />
      <Input
        label="Email"
        type="email"
        value={email}
        onChange={e => setEmail(e.target.value)}
        required
        autoComplete="email"
        placeholder="admin@company.com"
      />
      <Input
        label="Password"
        type="password"
        value={password}
        onChange={e => setPassword(e.target.value)}
        required
        minLength={8}
        placeholder="At least 8 characters"
      />
      <Button type="submit" size="lg" style={{ width: '100%', marginTop: 'var(--space-2)' }} disabled={loading || !name.trim()}>
        {loading ? 'Creating account...' : 'Create Admin Account'}
      </Button>
    </form>
  )
}
