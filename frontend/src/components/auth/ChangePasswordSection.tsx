import {useTranslation} from 'react-i18next'
import React, {useState, useCallback} from 'react'
import clsx from 'clsx'
import {Check, Circle, Eye, EyeOff, KeyRound} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {Button, Input} from '@/components/shared'
import {checkPassword, PASSWORD_CLASSES, PASSWORD_MAX_LENGTH, PASSWORD_MIN_LENGTH} from '@/lib/password'
import {SectionCard, SectionNotice} from './SectionCard'
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
  // Inline field validation: a persistent, screen-reader-announced error bound
  // to the field via the shared Input's aria-describedby wiring, instead of a
  // vanishing toast. The server's answer lands inline too.
  const [confirmError, setConfirmError] = useState('')
  const [submitError, setSubmitError] = useState('')

  // The rules are the server's (validatePasswordStrength), shown while typing.
  const check = checkPassword(newPassword)
  const matches = confirmPassword.length > 0 && confirmPassword === newPassword
  const canSubmit = currentPassword.length > 0 && check.valid && matches

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
        // The server revokes every refresh token of the account, this tab's
        // included, so the session would die at the next silent refresh
        // anyway. Go to the sign-in screen now, with the reason waiting there
        // (a toast would unmount with the shell).
        setPostLogoutNotice('password-changed')
        await logout()
      } catch (err) {
        setSubmitError(err instanceof Error ? err.message : t('password.updateFailed'))
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
            onChange={e => {
              setNewPassword(e.target.value)
              if (confirmError) setConfirmError('')
            }}
            placeholder={t('password.newPlaceholder')}
            autoComplete="new-password"
            required
            error={check.tooLong ? t('password.tooLong', {count: PASSWORD_MAX_LENGTH}) : undefined}
            aria-describedby={check.tooLong ? undefined : 'change-password-rules'}
            {...revealProps('new')}
          />
          <ul id="change-password-rules" aria-label={t('password.rulesLabel')} className="mt-2 space-y-1">
            <Rule met={check.longEnough}>{t('password.ruleLength', {count: PASSWORD_MIN_LENGTH})}</Rule>
            <Rule met={check.enoughClasses}>
              {t('password.ruleClasses')}
              <span className="inline-flex flex-wrap gap-1 ml-1.5 align-middle">
                {PASSWORD_CLASSES.map(c => (
                  <span
                    key={c}
                    className={clsx(
                      'px-1.5 rounded text-2xs font-medium leading-4',
                      check.classes[c]
                        ? 'bg-semantic-success/10 text-semantic-success'
                        : 'bg-surface-3 text-text-tertiary',
                    )}
                  >
                    {t(`passwordClass.${c}`)}
                  </span>
                ))}
              </span>
            </Rule>
          </ul>
        </div>
        <div>
          <label htmlFor="change-password-confirm" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('password.confirm')}
          </label>
          <Input
            id="change-password-confirm"
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

function Rule({met, children}: {met: boolean; children: React.ReactNode}) {
  const {t} = useTranslation('auth')
  const Icon = met ? Check : Circle
  return (
    <li
      className={clsx('flex items-start gap-1.5 text-xs', met ? 'text-text-secondary' : 'text-text-tertiary')}
      data-met={met || undefined}
    >
      <Icon
        size={13}
        className={clsx('mt-px shrink-0', met ? 'text-semantic-success' : 'text-text-disabled')}
        aria-hidden="true"
      />
      <span className="sr-only">{met ? t('password.ruleMet') : t('password.ruleUnmet')}</span>
      <span>{children}</span>
    </li>
  )
}
