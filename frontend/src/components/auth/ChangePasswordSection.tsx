import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import React, {useState, useCallback} from 'react'
import {Eye, EyeOff, KeyRound} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {Button, Input} from '@/components/shared'
import {checkPassword, PASSWORD_MAX_LENGTH} from '@/lib/password'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {SectionCard, SectionNotice} from './SectionCard'
import {PasswordRuleList} from './PasswordRuleList'
import {setPostLogoutNotice} from './authNotice'

type Field = 'current' | 'new' | 'confirm'

export const ChangePasswordSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const logout = useAuthStore(s => s.logout)
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [revealed, setRevealed] = useState<Record<Field, boolean>>({current: false, new: false, confirm: false})
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [submitError, setSubmitError] = useState('')

  const check = checkPassword(newPassword)
  const matches = confirmPassword.length > 0 && confirmPassword === newPassword
  const canSubmit = currentPassword.length > 0 && check.valid && matches
  const v = useFormValidation<'confirm'>({
    fields: ['confirm'],
    validate: () => {
      const errors: FieldErrors<'confirm'> = {}
      if (confirmPassword && newPassword !== confirmPassword) errors.confirm = t('password.mismatch')
      return errors
    },
  })

  const revealProps = (field: Field) => ({
    type: revealed[field] ? 'text' : 'password',
    trailingIcon: revealed[field] ? EyeOff : Eye,
    trailingLabel: revealed[field] ? t('password.hide') : t('password.show'),
    trailingPressed: revealed[field],
    onTrailingClick: () => setRevealed(r => ({...r, [field]: !r[field]})),
  })

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!canSubmit || isSubmitting) return
      setSubmitError('')
      setIsSubmitting(true)
      try {
        await authApi.changePassword(currentPassword, newPassword)
        setPostLogoutNotice('password-changed')
        await logout()
      } catch (err) {
        setSubmitError(getErrorMessage(err, t('password.updateFailed')))
        setIsSubmitting(false)
      }
    },
    [canSubmit, isSubmitting, currentPassword, newPassword, t, logout],
  )

  return (
    <SectionCard
      id="profile-password"
      icon={KeyRound}
      title={t('password.heading')}
      description={t('password.description')}
    >
      <form onSubmit={e => void handleSubmit(e)} className="space-y-4 max-w-md" noValidate>
        <div>
          <label htmlFor="change-password-current" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.current')}
          </label>
          <Input
            id="change-password-current"
            value={currentPassword}
            onChange={e => setCurrentPassword(e.target.value)}
            placeholder={t('password.currentPlaceholder')}
            autoComplete="current-password"
            required
            {...revealProps('current')}
          />
        </div>
        <div>
          <label htmlFor="change-password-new" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.new')}
          </label>
          <Input
            id="change-password-new"
            value={newPassword}
            onChange={e => setNewPassword(e.target.value)}
            placeholder={t('password.newPlaceholder')}
            autoComplete="new-password"
            required
            error={check.tooLong ? t('password.tooLong', {count: PASSWORD_MAX_LENGTH}) : undefined}
            aria-describedby={check.tooLong ? undefined : 'change-password-rules'}
            {...revealProps('new')}
          />
          <PasswordRuleList id="change-password-rules" check={check} />
        </div>
        <div>
          <label htmlFor="change-password-confirm" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.confirm')}
          </label>
          <Input
            id="change-password-confirm"
            value={confirmPassword}
            onChange={e => setConfirmPassword(e.target.value)}
            error={v.errorFor('confirm')}
            onBlur={() => v.touch('confirm')}
            placeholder={t('password.confirmPlaceholder')}
            autoComplete="new-password"
            required
            {...revealProps('confirm')}
          />
        </div>

        {submitError && <SectionNotice message={submitError} />}

        <Button type="submit" variant="primary" size="md" disabled={!canSubmit} loading={isSubmitting}>
          {t('password.submit')}
        </Button>
      </form>
    </SectionCard>
  )
}
