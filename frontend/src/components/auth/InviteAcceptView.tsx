import {useEffect, useState} from 'react'
import {CheckCircle2, XCircle} from 'lucide-react'
import Button from '@/components/shared/Button'
import Spinner from '@/components/shared/Spinner'
import {useOrgStore} from '@/stores/orgStore'

interface InviteAcceptViewProps {
  token: string
  onDone: () => void
}

// InviteAcceptView consumes an #invite=<token> deep link for an authenticated
// user: it redeems the token (POST /api/invites/{token}/accept via the org
// store) on mount and shows the result. Acceptance requires a logged-in user,
// so ProtectedRoute only renders this once authenticated.
export default function InviteAcceptView({token, onDone}: InviteAcceptViewProps) {
  const acceptInvite = useOrgStore(s => s.acceptInvite)
  const [status, setStatus] = useState<'pending' | 'ok' | 'error'>('pending')
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let active = true
    acceptInvite(token)
      .then(() => {
        if (active) setStatus('ok')
      })
      .catch((err: unknown) => {
        if (!active) return
        setError(err instanceof Error ? err.message : 'Could not accept the invitation')
        setStatus('error')
      })
    return () => {
      active = false
    }
  }, [token, acceptInvite])

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <div className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col items-center gap-4 text-center">
        {status === 'pending' && (
          <>
            <Spinner size={32} />
            <p className="text-sm text-text-muted">Accepting your invitation…</p>
          </>
        )}
        {status === 'ok' && (
          <>
            <CheckCircle2 className="text-semantic-success" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">Invitation accepted</h1>
            <p className="text-sm text-text-muted">You've joined the organization.</p>
            <Button type="button" variant="primary" fullWidth onClick={onDone}>
              Continue
            </Button>
          </>
        )}
        {status === 'error' && (
          <>
            <XCircle className="text-semantic-error" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">Invitation failed</h1>
            <p className="text-sm text-text-muted">{error ?? 'This invite is invalid or has expired.'}</p>
            <Button type="button" variant="secondary" fullWidth onClick={onDone}>
              Continue
            </Button>
          </>
        )}
      </div>
    </div>
  )
}
