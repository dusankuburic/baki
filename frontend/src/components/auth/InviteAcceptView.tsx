import {useTranslation} from 'react-i18next'
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
  const {t} = useTranslation('auth')
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
        setError(err instanceof Error ? err.message : null)
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
            <p className="text-sm text-text-tertiary">{t('invite.pending')}</p>
          </>
        )}
        {status === 'ok' && (
          <>
            <CheckCircle2 className="text-semantic-success" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">{t('invite.okTitle')}</h1>
            <p className="text-sm text-text-tertiary">{t('invite.okBody')}</p>
            <Button type="button" variant="primary" fullWidth onClick={onDone}>
              {t('invite.continue')}
            </Button>
          </>
        )}
        {status === 'error' && (
          <>
            <XCircle className="text-semantic-error" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">{t('invite.errorTitle')}</h1>
            <p className="text-sm text-text-tertiary">{error ?? t('invite.errorBody')}</p>
            <Button type="button" variant="secondary" fullWidth onClick={onDone}>
              {t('invite.continue')}
            </Button>
          </>
        )}
      </div>
    </div>
  )
}
