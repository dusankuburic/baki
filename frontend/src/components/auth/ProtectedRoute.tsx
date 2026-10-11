import {useEffect, useState, type ReactNode} from 'react'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'
import LoginForm from './LoginForm'
import ResetPasswordView from './ResetPasswordView'
import VerifyEmailView from './VerifyEmailView'
import InviteAcceptView from './InviteAcceptView'
import {parseRecoveryHash, clearRecoveryHash, type RecoveryHash} from './authHash'
import Spinner from '@/components/shared/Spinner'

interface ProtectedRouteProps {
  children: ReactNode
}

export default function ProtectedRoute({children}: ProtectedRouteProps) {
  const isAuthenticated = useAuthStore(s => s.isAuthenticated)
  const isBooting = useAuthStore(s => s.isBooting)
  const loadFromStorage = useAuthStore(s => s.loadFromStorage)
  const loadOrgs = useOrgStore(s => s.loadOrgs)
  const [recovery, setRecovery] = useState<RecoveryHash>(() => (isTauri() ? {} : parseRecoveryHash()))
  const clearRecovery = () => setRecovery({})

  useEffect(() => {
    if (recovery.resetToken || recovery.verifyToken || recovery.inviteToken) clearRecoveryHash()
  }, [recovery.resetToken, recovery.verifyToken, recovery.inviteToken])

  useEffect(() => {
    if (isAuthenticated && !isTauri() && window.location.hash.includes('ssoLinked=')) {
      useUIStore.getState().setMainPaneView('profile')
    }
  }, [isAuthenticated])

  useEffect(() => {
    if (!isTauri()) {
      void loadFromStorage()
    }
  }, [loadFromStorage])

  useEffect(() => {
    if (!isTauri() && isAuthenticated) {
      void loadOrgs()
    }
  }, [isAuthenticated, loadOrgs])

  if (isTauri()) {
    return <>{children}</>
  }

  if (recovery.resetToken) {
    return <ResetPasswordView token={recovery.resetToken} onDone={clearRecovery} />
  }
  if (recovery.verifyToken) {
    return <VerifyEmailView token={recovery.verifyToken} onDone={clearRecovery} />
  }

  if (isBooting) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-1">
        <Spinner size={32} />
      </div>
    )
  }

  if (!isAuthenticated) {
    return <LoginForm />
  }

  if (recovery.inviteToken) {
    return <InviteAcceptView token={recovery.inviteToken} onDone={clearRecovery} />
  }

  return <>{children}</>
}
