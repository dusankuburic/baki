import { useEffect, type ReactNode } from 'react'
import { useAuthStore } from '@/stores/authStore'
import { useOrgStore } from '@/stores/orgStore'
import { isTauri } from '@/platform/guards'
import LoginForm from './LoginForm'
import Spinner from '@/components/shared/Spinner'

interface ProtectedRouteProps {
  children: ReactNode
}

// ProtectedRoute renders its children when the user is authenticated.
// In Tauri (desktop) mode it skips auth entirely because the backend is
// local and protected by the per-session token.
export default function ProtectedRoute({ children }: ProtectedRouteProps) {
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const isLoading = useAuthStore(s => s.isLoading)
  const loadFromStorage = useAuthStore(s => s.loadFromStorage)
  const loadOrgs = useOrgStore(s => s.loadOrgs)

  useEffect(() => {
    if (!isTauri()) {
      loadFromStorage()
    }
  }, [loadFromStorage])

  useEffect(() => {
    if (!isTauri() && isAuthenticated) {
      loadOrgs()
    }
  }, [isAuthenticated, loadOrgs])

  // Desktop: skip auth gate
  if (isTauri()) {
    return <>{children}</>
  }

  // Web: show spinner while resolving token from storage
  if (isLoading) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-1">
        <Spinner size={32} />
      </div>
    )
  }

  if (!isAuthenticated) {
    return <LoginForm />
  }

  return <>{children}</>
}
