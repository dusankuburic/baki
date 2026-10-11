import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {Lock, KeyRound, CheckCircle2} from 'lucide-react'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {authApi} from '@/api/auth'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {checkPassword, PASSWORD_MAX_LENGTH} from '@/lib/password'
import {PasswordRuleList} from './PasswordRuleList'

interface ResetPasswordViewProps {
  token: string
  onDone: () => void
}

type ResetField = 'password' | 'confirm'
const RESET_FIELDS = ['password', 'confirm'] as const satisfies readonly ResetField[]

export default function ResetPasswordView({token, onDone}: ResetPasswordViewProps) {
  const {t} = useTranslation('auth')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [done, setDone] = useState(false)

  const check = checkPassword(password)
  const v = useFormValidation<ResetField>({
    fields: RESET_FIELDS,
    validate: () => {
      const errors: FieldErrors<ResetField> = {}
      if (check.tooLong) errors.password = t('password.tooLong', {count: PASSWORD_MAX_LENGTH})
      if (confirm && password !== confirm) errors.confirm = t('reset.mismatch')
      return errors
    },
  })

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)
    const invalid = v.submitValidate()
    if (invalid) {
      document.getElementById(invalid === 'password' ? 'new-password' : 'confirm-password')?.focus()
      return
    }
    setLoading(true)
    try {
      await authApi.resetPassword(token, password)
      setDone(true)
    } catch (err) {
      setError(getErrorMessage(err, t('reset.failed')))
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
          <form onSubmit={handleSubmit} noValidate className="flex flex-col gap-5">
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
              {t('reset.newPasswordLabel')}
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
              error={v.errorFor('password')}
              aria-describedby={v.errorFor('password') ? undefined : 'reset-password-rules'}
              onBlur={() => v.touch('password')}
            />
            <PasswordRuleList id="reset-password-rules" check={check} />
            <label htmlFor="confirm-password" className="text-xs font-medium text-text-secondary">
              {t('reset.confirmPasswordLabel')}
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
              {...v.fieldProps('confirm')}
            />

            <Button
              type="submit"
              variant="primary"
              fullWidth
              loading={loading}
              icon={KeyRound}
              disabled={loading || !password || !confirm || !check.valid}
            >
              {t('reset.submit')}
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
