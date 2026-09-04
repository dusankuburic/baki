import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {Lock, KeyRound, CheckCircle2} from 'lucide-react'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {authApi} from '@/api/auth'

interface ResetPasswordViewProps {
  token: string
  // onDone returns the user to the login screen (clears the recovery view).
  onDone: () => void
}

// ResetPasswordView consumes a #resetPassword=<token> deep link: it collects a
// new password and submits it with the single-use token. The backend enforces
// the full password policy and returns a message on failure.
export default function ResetPasswordView({token, onDone}: ResetPasswordViewProps) {
  const {t} = useTranslation('auth')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [done, setDone] = useState(false)

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)
    if (password !== confirm) {
      setError(t('reset.mismatch'))
      return
    }
    if (password.length < 12) {
      setError('Password must be at least 12 characters')
      return
    }
    setLoading(true)
    try {
      await authApi.resetPassword(token, password)
      setDone(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : t('reset.failed'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <div className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5">
        {done ? (
          <>
            <div className="flex flex-col items-center gap-3 text-center">
              <CheckCircle2 className="text-semantic-success" size={40} />
              <h1 className="text-xl font-semibold text-text-primary">{t('reset.doneTitle')}</h1>
              <p className="text-sm text-text-tertiary">{t('reset.doneBody')}</p>
            </div>
            <Button type="button" variant="primary" fullWidth onClick={onDone}>
              {t('reset.backToSignIn')}
            </Button>
          </>
        ) : (
          <form onSubmit={handleSubmit} className="flex flex-col gap-5">
            <div className="flex flex-col gap-1 mb-2">
              <h1 className="text-xl font-semibold text-text-primary">{t('reset.formTitle')}</h1>
              <p className="text-sm text-text-tertiary">{t('reset.formSubtitle')}</p>
            </div>

            {error && (
              <div
                role="alert"
                className="px-3 py-2 bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-sm text-semantic-error"
              >
                {error}
              </div>
            )}

            <label htmlFor="new-password" className="text-xs font-medium text-text-secondary">
              New password
            </label>
            <Input
              id="new-password"
              type="password"
              placeholder={t('reset.newPassword')}
              icon={Lock}
              value={password}
              onChange={e => setPassword(e.target.value)}
              required
              autoComplete="new-password"
              autoFocus
            />
            <label htmlFor="confirm-password" className="text-xs font-medium text-text-secondary">
              Confirm new password
            </label>
            <Input
              id="confirm-password"
              type="password"
              placeholder={t('reset.confirmPassword')}
              icon={Lock}
              value={confirm}
              onChange={e => setConfirm(e.target.value)}
              required
              autoComplete="new-password"
            />

            <Button
              type="submit"
              variant="primary"
              fullWidth
              loading={loading}
              icon={KeyRound}
              disabled={loading || !password || !confirm}
            >
              Reset password
            </Button>

            <div className="text-center text-sm text-text-tertiary">
              <button type="button" className="text-brand-500 hover:underline font-medium" onClick={onDone}>
                {t('reset.backToSignIn')}
              </button>
            </div>
          </form>
        )}
      </div>
    </div>
  )
}
