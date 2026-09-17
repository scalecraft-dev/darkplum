import { useState } from 'react'
import { useAuth } from '../context/AuthContext'
import { useTheme } from '../context/ThemeContext'
import { CopyButton } from '../components/ui/CopyButton'
import { Avatar, Button, Input } from '../components/ui'
import { changePassword } from '../lib/api'
import styles from './ProfilePage.module.css'

const DEPLOYMENT_ID = 'dp-' + Math.random().toString(36).slice(2, 10)

function ChangePasswordForm() {
  const [currentPw, setCurrentPw] = useState('')
  const [newPw, setNewPw] = useState('')
  const [message, setMessage] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setMessage('')
    if (newPw.length < 8) { setError('New password must be at least 8 characters'); return }
    setLoading(true)
    try {
      await changePassword(currentPw, newPw)
      setMessage('Password changed successfully')
      setCurrentPw('')
      setNewPw('')
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <form onSubmit={handleSubmit}>
      {error && <div className={styles.formError}>{error}</div>}
      {message && <div className={styles.formSuccess}>{message}</div>}
      <div className={styles.row}>
        <div className={styles.rowLabel}>
          <span className={styles.rowTitle}>Current Password</span>
        </div>
        <Input
          type="password"
          label="Current Password"
          value={currentPw}
          onChange={e => setCurrentPw(e.target.value)}
          required
          className={styles.passwordInput}
        />
      </div>
      <div className={styles.row}>
        <div className={styles.rowLabel}>
          <span className={styles.rowTitle}>New Password</span>
          <span className={styles.rowDesc}>At least 8 characters</span>
        </div>
        <Input
          type="password"
          label="New Password"
          value={newPw}
          onChange={e => setNewPw(e.target.value)}
          required
          minLength={8}
          className={styles.passwordInput}
        />
      </div>
      <div className={`${styles.row} ${styles.rowEnd}`}>
        <Button type="submit" size="sm" disabled={loading}>
          {loading ? 'Changing...' : 'Change Password'}
        </Button>
      </div>
    </form>
  )
}

export function ProfilePage() {
  const { user } = useAuth()
  const { theme, toggleTheme } = useTheme()

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <h1 className={styles.title}>Profile</h1>
        <p className={styles.subtitle}>Account settings and preferences</p>
      </div>

      <div className={styles.sections}>
        {user && (
          <section className={styles.card}>
            <div className={styles.accountHeader}>
              <Avatar name={user.name || user.email} size="md" src={user.picture} />
              <h2 className={styles.cardTitle}>Account</h2>
            </div>
            <div className={styles.row}>
              <div className={styles.rowLabel}>
                <span className={styles.rowTitle}>Name</span>
              </div>
              <span>{user.name}</span>
            </div>
            <div className={styles.row}>
              <div className={styles.rowLabel}>
                <span className={styles.rowTitle}>Email</span>
              </div>
              <span className={styles.mono}>{user.email}</span>
            </div>
          </section>
        )}

        {user?.has_password && (
          <section className={styles.card}>
            <h2 className={styles.cardTitle}>Change Password</h2>
            <ChangePasswordForm />
          </section>
        )}

        <section className={styles.card}>
          <h2 className={styles.cardTitle}>Appearance</h2>
          <div className={styles.row}>
            <div className={styles.rowLabel}>
              <span className={styles.rowTitle}>Theme</span>
              <span className={styles.rowDesc}>Choose between light and dark mode</span>
            </div>
            <div className={styles.themeToggle}>
              <button
                className={`${styles.themeOption} ${theme === 'light' ? styles.themeActive : ''}`}
                onClick={() => { if (theme !== 'light') toggleTheme() }}
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <circle cx="12" cy="12" r="5" />
                  <line x1="12" y1="1" x2="12" y2="3" />
                  <line x1="12" y1="21" x2="12" y2="23" />
                  <line x1="4.22" y1="4.22" x2="5.64" y2="5.64" />
                  <line x1="18.36" y1="18.36" x2="19.78" y2="19.78" />
                  <line x1="1" y1="12" x2="3" y2="12" />
                  <line x1="21" y1="12" x2="23" y2="12" />
                  <line x1="4.22" y1="19.78" x2="5.64" y2="18.36" />
                  <line x1="18.36" y1="5.64" x2="19.78" y2="4.22" />
                </svg>
                Light
              </button>
              <button
                className={`${styles.themeOption} ${theme === 'dark' ? styles.themeActive : ''}`}
                onClick={() => { if (theme !== 'dark') toggleTheme() }}
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
                </svg>
                Dark
              </button>
            </div>
          </div>
        </section>

        <section className={styles.card}>
          <h2 className={styles.cardTitle}>Deployment</h2>
          <div className={styles.row}>
            <div className={styles.rowLabel}>
              <span className={styles.rowTitle}>Status</span>
            </div>
            <div className={styles.statusBadge}>
              <span className={styles.statusDot} />
              Running
            </div>
          </div>
          <div className={styles.row}>
            <div className={styles.rowLabel}>
              <span className={styles.rowTitle}>Deployment ID</span>
            </div>
            <div className={styles.idValue}>
              <code className={styles.mono}>{DEPLOYMENT_ID}</code>
              <CopyButton value={DEPLOYMENT_ID} />
            </div>
          </div>
          <div className={styles.row}>
            <div className={styles.rowLabel}>
              <span className={styles.rowTitle}>Version</span>
            </div>
            <span className={styles.mono}>0.1.0</span>
          </div>
          <div className={styles.row}>
            <div className={styles.rowLabel}>
              <span className={styles.rowTitle}>Infrastructure</span>
            </div>
            <span className={styles.valueText}>Self-hosted (your cloud account)</span>
          </div>
        </section>
      </div>
    </div>
  )
}
