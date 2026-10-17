import { Navigate, Route, Routes } from 'react-router-dom'
import { Layout } from './components/Layout'
import { ProtectedRoute } from './components/ProtectedRoute'
import { ActivityPage } from './pages/ActivityPage'
import { AdminPage } from './pages/AdminPage'
import { ChatPage } from './pages/ChatPage'
import { LoginPage } from './pages/LoginPage'
import { ModelsPage } from './pages/ModelsPage'
import { ProfilePage } from './pages/ProfilePage'
import { RoutingPage } from './pages/RoutingPage'

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/" element={<Navigate to="/chat" replace />} />
      <Route path="/admin" element={<ProtectedRoute requireAdmin><Navigate to="/admin/users" replace /></ProtectedRoute>} />
      <Route path="/admin/users" element={<ProtectedRoute requireAdmin><Layout><AdminPage /></Layout></ProtectedRoute>} />
      <Route path="/admin/models" element={<ProtectedRoute requireAdmin><Layout><ModelsPage /></Layout></ProtectedRoute>} />
      <Route path="/admin/routing" element={<ProtectedRoute requireAdmin><Layout><RoutingPage /></Layout></ProtectedRoute>} />
      <Route path="/admin/activity" element={<ProtectedRoute requireAdmin><Layout><ActivityPage /></Layout></ProtectedRoute>} />
      <Route path="/chat" element={<ProtectedRoute><Layout><ChatPage /></Layout></ProtectedRoute>} />
      <Route path="/profile" element={<ProtectedRoute><Layout><ProfilePage /></Layout></ProtectedRoute>} />
    </Routes>
  )
}
