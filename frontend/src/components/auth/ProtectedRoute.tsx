import { useEffect, useState, type ReactNode } from 'react'
import { useAuthStore } from '@/stores/authStore'
import { useOrgStore } from '@/stores/orgStore'
import { isTauri } from '@/platform/guards'
import LoginForm from './LoginForm'
import ResetPasswordView from './ResetPasswordView'
import VerifyEmailView from './VerifyEmailView'
import InviteAcceptView from './InviteAcceptView'
import { parseRecoveryHash, clearRecoveryHash, type RecoveryHash } from './authHash'
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
  // Read account-recovery deep links once (web only). When present they take
  // precedence over the login gate so a logged-out user can still act on them.
  // Parsing is pure (StrictMode-safe); the fragment is stripped in an effect.
  const [recovery, setRecovery] = useState<RecoveryHash>(() => (isTauri() ? {} : parseRecoveryHash()))
  const clearRecovery = () => setRecovery({})

  useEffect(() => {
    if (recovery.resetToken || recovery.verifyToken || recovery.inviteToken) clearRecoveryHash()
  }, [recovery.resetToken, recovery.verifyToken, recovery.inviteToken])

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

  // Account-recovery deep links (from emailed links) render before everything
  // else, including the auth gate.
  if (recovery.resetToken) {
    return <ResetPasswordView token={recovery.resetToken} onDone={clearRecovery} />
  }
  if (recovery.verifyToken) {
    return <VerifyEmailView token={recovery.verifyToken} onDone={clearRecovery} />
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
    // An invite link reached a logged-out user: show login first. The token is
    // held in state across the auth change, so after sign-in this component
    // re-renders and falls through to InviteAcceptView below.
    return <LoginForm />
  }

  // Org invite acceptance requires an authenticated user (the backend adds the
  // caller to the org), so it runs after the auth gate, unlike reset/verify.
  if (recovery.inviteToken) {
    return <InviteAcceptView token={recovery.inviteToken} onDone={clearRecovery} />
  }

  return <>{children}</>
}
