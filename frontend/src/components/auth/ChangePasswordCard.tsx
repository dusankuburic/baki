import {useTranslation} from 'react-i18next'
import React, {useState, useCallback} from 'react'
import {KeyRound} from 'lucide-react'
import {authApi} from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {useToast} from '@/components/shared'

export const ChangePasswordCard: React.FC = () => {
  const {t} = useTranslation('auth')
  const toast = useToast()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)
  // Inline field validation (B6): a persistent, screen-reader-announced error
  // bound to the field via the shared Input's aria-describedby wiring, instead
  // of a vanishing toast.
  const [confirmError, setConfirmError] = useState('')

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (newPassword !== confirmPassword) {
        setConfirmError(t('password.mismatch'))
        return
      }
      setConfirmError('')
      setIsSubmitting(true)
      try {
        await authApi.changePassword(currentPassword, newPassword)
        toast.success(t('password.updated'))
        setCurrentPassword('')
        setNewPassword('')
        setConfirmPassword('')
      } catch (err) {
        toast.error(t('password.updateFailed'), {description: err instanceof Error ? err.message : String(err)})
      } finally {
        setIsSubmitting(false)
      }
    },
    [currentPassword, newPassword, confirmPassword, toast, t],
  )

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
        <KeyRound size={13} className="text-text-tertiary" />
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">{t('password.heading')}</h2>
      </div>

      <form onSubmit={handleSubmit} className="p-5 space-y-3">
        <div>
          <label htmlFor="change-password-current" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.current')}
          </label>
          <Input
            id="change-password-current"
            type="password"
            value={currentPassword}
            onChange={e => setCurrentPassword(e.target.value)}
            placeholder={t('password.currentPlaceholder')}
            autoComplete="current-password"
            required
          />
        </div>
        <div>
          <label htmlFor="change-password-new" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.new')}
          </label>
          <Input
            id="change-password-new"
            type="password"
            value={newPassword}
            onChange={e => setNewPassword(e.target.value)}
            placeholder={t('password.newPlaceholder')}
            autoComplete="new-password"
            required
          />
        </div>
        <div>
          <label htmlFor="change-password-confirm" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.confirm')}
          </label>
          <Input
            id="change-password-confirm"
            type="password"
            value={confirmPassword}
            onChange={e => {
              setConfirmPassword(e.target.value)
              if (confirmError) setConfirmError('')
            }}
            onBlur={() => {
              if (confirmPassword && newPassword !== confirmPassword) setConfirmError(t('password.mismatch'))
            }}
            error={confirmError || undefined}
            placeholder={t('password.confirmPlaceholder')}
            autoComplete="new-password"
            required
          />
        </div>

        {confirmError && (
          <div role="alert" className="text-xs text-semantic-error">
            {confirmError}
          </div>
        )}

        <Button type="submit" variant="primary" size="md" fullWidth loading={isSubmitting}>
          {t('password.submit')}
        </Button>
      </form>
    </div>
  )
}
