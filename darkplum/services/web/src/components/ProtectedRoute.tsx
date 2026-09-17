import { useEffect } from 'react'
import { Navigate, useNavigate, useLocation } from 'react-router-dom'
import type { ReactNode } from 'react'
import { useAuth } from '../context/AuthContext'

interface ProtectedRouteProps {
  children: ReactNode
  requireAdmin?: boolean
}

export function ProtectedRoute({ children, requireAdmin }: ProtectedRouteProps) {
  const { user, loading } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()

  useEffect(() => {
    if (!loading && !user) {
      navigate('/login', { state: { returnTo: location.pathname }, replace: true })
    }
  }, [loading, user, navigate, location.pathname])

  if (loading) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
        <span role="status" aria-live="polite">Loading...</span>
      </div>
    )
  }

  if (!user) return null

  if (requireAdmin && user.role !== 'admin') {
    return <Navigate to="/chat" replace />
  }

  return <>{children}</>
}
