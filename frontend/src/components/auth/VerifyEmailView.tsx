import { useEffect, useState } from 'react'
import { CheckCircle2, XCircle } from 'lucide-react'
import Button from '@/components/shared/Button'
import Spinner from '@/components/shared/Spinner'
import { authApi } from '@/api/auth'

interface VerifyEmailViewProps {
  token: string
  onDone: () => void
}

// VerifyEmailView consumes a #verifyEmail=<token> deep link: it redeems the
// token on mount and shows the result. The effect runs once for the given token.
export default function VerifyEmailView({ token, onDone }: VerifyEmailViewProps) {
  const [status, setStatus] = useState<'pending' | 'ok' | 'error'>('pending')
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let active = true
    authApi.verifyEmail(token)
      .then(() => { if (active) setStatus('ok') })
      .catch((err: unknown) => {
        if (!active) return
        setError(err instanceof Error ? err.message : 'Verification failed')
        setStatus('error')
      })
    return () => { active = false }
  }, [token])

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <div className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col items-center gap-4 text-center">
        {status === 'pending' && (
          <>
            <Spinner size={32} />
            <p className="text-sm text-text-muted">Verifying your email…</p>
          </>
        )}
        {status === 'ok' && (
          <>
            <CheckCircle2 className="text-semantic-success" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">Email verified</h1>
            <p className="text-sm text-text-muted">Thanks — your email address is now confirmed.</p>
            <Button type="button" variant="primary" fullWidth onClick={onDone}>
              Continue to sign in
            </Button>
          </>
        )}
        {status === 'error' && (
          <>
            <XCircle className="text-semantic-error" size={40} />
            <h1 className="text-xl font-semibold text-text-primary">Verification failed</h1>
            <p className="text-sm text-text-muted">{error ?? 'This link is invalid or has expired.'}</p>
            <Button type="button" variant="secondary" fullWidth onClick={onDone}>
              Back to sign in
            </Button>
          </>
        )}
      </div>
    </div>
  )
}
