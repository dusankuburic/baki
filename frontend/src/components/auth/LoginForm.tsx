import {useEffect, useRef, useState} from 'react'
import {Mail, Lock, LogIn, UserPlus, KeyRound, Send} from 'lucide-react'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {useAuthStore} from '@/stores/authStore'
import {authApi, type SSOInfo} from '@/api/auth'
import {systemApi} from '@/api/system'
import {ApiError, getBackendConfig} from '@/api/client'
import {useTranslation} from 'react-i18next'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {emailErrorKey, type CommonFieldErrorKey} from '@/lib/validators'
import {EMAIL_MAX_LENGTH} from '@/lib/constraints'
import {checkPassword, PASSWORD_MAX_LENGTH} from '@/lib/password'
import {PasswordRuleList} from './PasswordRuleList'
import {clearPostLogoutNotice, readPostLogoutNotice} from './authNotice'

interface LoginFormProps {
  onSuccess?: () => void
}

type LoginField = 'email' | 'password' | 'confirmPassword'
const LOGIN_FIELDS = ['email', 'password', 'confirmPassword'] as const satisfies readonly LoginField[]

const FIELD_ELEMENT_ID: Record<LoginField, string> = {
  email: 'email',
  password: 'password',
  confirmPassword: 'confirm-password',
}

function readSSOHash(): {ticket?: string; error?: string} {
  const hash = window.location.hash.replace(/^#/, '')
  if (!hash.includes('ssoTicket=') && !hash.includes('ssoError=')) return {}
  const params = new URLSearchParams(hash)
  const out = {
    ticket: params.get('ssoTicket') ?? undefined,
    error: params.get('ssoError') ?? undefined,
  }
  history.replaceState(null, '', window.location.pathname + window.location.search)
  return out
}

export default function LoginForm({onSuccess}: LoginFormProps) {
  const {t} = useTranslation('auth')
  const [isRegister, setIsRegister] = useState(false)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [remember, setRemember] = useState(false)
  const [sso, setSso] = useState<SSOInfo | null>(null)
  const [signUpDisabled, setSignUpDisabled] = useState(false)
  const [ssoError, setSsoError] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [forgot, setForgot] = useState(false)
  const [forgotSent, setForgotSent] = useState(false)
  const [forgotLoading, setForgotLoading] = useState(false)
  const [registeredEmail, setRegisteredEmail] = useState<string | null>(null)
  const [needsVerification, setNeedsVerification] = useState(false)
  const [resendState, setResendState] = useState<'idle' | 'sending' | 'sent'>('idle')
  const [notice] = useState(readPostLogoutNotice)
  const onSuccessRef = useRef(onSuccess)
  useEffect(() => {
    onSuccessRef.current = onSuccess
  })
  useEffect(() => {
    clearPostLogoutNotice()
  }, [])
  const login = useAuthStore(s => s.login)
  const register = useAuthStore(s => s.register)
  const loginWithSSOTicket = useAuthStore(s => s.loginWithSSOTicket)
  const isLoading = useAuthStore(s => s.isLoading)
  const error = useAuthStore(s => s.error)
  const clearError = useAuthStore(s => s.clearError)

  const check = checkPassword(password)
  const emailInvalid = (key: CommonFieldErrorKey): string => t(key, {ns: 'common', max: EMAIL_MAX_LENGTH})
  const v = useFormValidation<LoginField>({
    fields: LOGIN_FIELDS,
    validate: () => {
      const errors: FieldErrors<LoginField> = {}
      const emailKey = emailErrorKey(email)
      if (emailKey) errors.email = emailInvalid(emailKey)
      if (check.tooLong) errors.password = t('password.tooLong', {count: PASSWORD_MAX_LENGTH})
      if (isRegister && confirmPassword && password !== confirmPassword)
        errors.confirmPassword = t('login.passwordsDoNotMatch')
      return errors
    },
  })
  const canSubmit =
    email.trim() !== '' &&
    password !== '' &&
    (!isRegister ||
      (confirmPassword !== '' && check.valid && emailErrorKey(email) === null))

  useEffect(() => {
    let cancelled = false
    authApi
      .ssoInfo()
      .then(info => {
        if (!cancelled) setSso(info)
      })
      .catch(() => {
        if (!cancelled) setSso(null)
      })
    void (async () => {
      try {
        const features = await systemApi.features()
        if (!cancelled && features?.disableSignUp) {
          setSignUpDisabled(true)
          setIsRegister(false)
        }
      } catch {
      }
    })()

    const {ticket, error: hashError} = readSSOHash()
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (hashError) setSsoError(hashError)
    if (ticket) {
      loginWithSSOTicket(ticket)
        .then(() => {
          if (!cancelled) onSuccessRef.current?.()
        })
        .catch(() => {
        })
    }
    return () => {
      cancelled = true
    }
  }, [loginWithSSOTicket])

  async function handleSSOStart() {
    clearError()
    setSsoError(null)
    const cfg = await getBackendConfig()
    window.location.href = `${cfg.apiUrl}/api/auth/sso/start`
  }

  async function handleForgotSubmit(e: React.FormEvent) {
    e.preventDefault()
    setFormError(null)
    const emailKey = emailErrorKey(email)
    if (emailKey) {
      setFormError(emailInvalid(emailKey))
      return
    }
    setForgotLoading(true)
    try {
      await authApi.forgotPassword(email)
      setForgotSent(true)
    } catch {
      setForgotSent(true)
    } finally {
      setForgotLoading(false)
    }
  }

  function backToLogin() {
    setForgot(false)
    setForgotSent(false)
    setRegisteredEmail(null)
    setNeedsVerification(false)
    setResendState('idle')
    setIsRegister(false)
    setFormError(null)
    clearError()
  }

  async function handleResend() {
    setResendState('sending')
    try {
      await authApi.resendVerification(email)
    } catch {
    } finally {
      setResendState('sent')
    }
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    clearError()
    setFormError(null)
    setNeedsVerification(false)

    const invalid = v.submitValidate()
    if (invalid) {
      document.getElementById(FIELD_ELEMENT_ID[invalid])?.focus()
      return
    }

    try {
      if (isRegister) {
        const outcome = await register({email, password}, remember)
        if (outcome === 'verification-required') {
          setRegisteredEmail(email)
          return
        }
      } else {
        await login({email, password}, remember)
      }
      onSuccess?.()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'email_not_verified') setNeedsVerification(true)
    }
  }

  if (registeredEmail) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-1">
        <div
          role="status"
          className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5"
        >
          <div className="flex flex-col gap-1 mb-2">
            <h1 className="text-xl font-semibold text-text-primary">{t('login.registeredTitle')}</h1>
            <p className="text-sm text-text-tertiary">{t('login.registeredBody', {email: registeredEmail})}</p>
          </div>
          <p className="text-sm text-text-tertiary">
            {resendState === 'sent' ? t('login.resendSent') : t('login.notVerifiedHint')}
          </p>
          {resendState !== 'sent' && (
            <Button
              type="button"
              variant="secondary"
              fullWidth
              icon={Send}
              loading={resendState === 'sending'}
              disabled={resendState === 'sending'}
              onClick={handleResend}
            >
              {t('login.resend')}
            </Button>
          )}
          <div className="text-center text-sm text-text-tertiary">
            <button type="button" className="text-brand-500 hover:underline font-medium" onClick={backToLogin}>
              {t('login.backToSignIn')}
            </button>
          </div>
        </div>
      </div>
    )
  }

  if (forgot) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <form
        onSubmit={handleForgotSubmit}
        noValidate
        className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5"
      >
          <div className="flex flex-col gap-1 mb-2">
            <h1 className="text-xl font-semibold text-text-primary">{t('login.resetTitle')}</h1>
            <p className="text-sm text-text-tertiary">{forgotSent ? t('login.resetSent') : t('login.resetPrompt')}</p>
          </div>

          {!forgotSent && (
            <>
              <div className="flex flex-col gap-1.5">
                <label htmlFor="forgot-email" className="text-xs font-medium text-text-secondary">
                  {t('login.email')}
                </label>
                <Input
                  id="forgot-email"
                  type="email"
                  placeholder={t('login.email')}
                  icon={Mail}
                  value={email}
                  onChange={e => setEmail(e.target.value)}
                  required
                  autoComplete="email"
                  autoFocus
                />
              </div>
              <Button
                type="submit"
                variant="primary"
                fullWidth
                loading={forgotLoading}
                icon={Send}
                disabled={forgotLoading || !email}
              >
                {t('login.resetSubmit')}
              </Button>
            </>
          )}

          <div className="text-center text-sm text-text-tertiary">
            <button type="button" className="text-brand-500 hover:underline font-medium" onClick={backToLogin}>
              {t('login.backToSignIn')}
            </button>
          </div>
        </form>
      </div>
    )
  }

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <form
        onSubmit={handleSubmit}
        noValidate
        className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5"
      >
        <div className="flex flex-col gap-1 mb-2">
          <h1 className="text-xl font-semibold text-text-primary">
            {isRegister ? t('login.titleRegister') : t('login.titleSignIn')}
          </h1>
          <p className="text-sm text-text-tertiary">
            {isRegister ? t('login.subtitleRegister') : t('login.subtitleSignIn')}
          </p>
        </div>

        {notice && !isRegister && (
          <div
            role="status"
            className="px-3 py-2 bg-semantic-success/10 border border-semantic-success/30 rounded-lg text-sm text-semantic-success"
          >
            {notice === 'password-changed' ? t('login.noticePasswordChanged') : t('login.noticeAccountDeleted')}
          </div>
        )}

        {(error || ssoError || formError) && (
          <div
            role="alert"
            className="px-3 py-2 bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-sm text-semantic-error"
          >
            {error || ssoError || formError}
          </div>
        )}

        {needsVerification && (
          <div className="flex flex-col gap-2 -mt-2 text-sm text-text-tertiary">
            <span>{resendState === 'sent' ? t('login.resendSent') : t('login.notVerifiedHint')}</span>
            {resendState !== 'sent' && (
              <Button
                type="button"
                variant="secondary"
                fullWidth
                icon={Send}
                loading={resendState === 'sending'}
                disabled={resendState === 'sending' || !email}
                onClick={handleResend}
              >
                {t('login.resend')}
              </Button>
            )}
          </div>
        )}

        <div className="flex flex-col gap-1.5">
          <label htmlFor="email" className="text-xs font-medium text-text-secondary">
            {t('login.email')}
          </label>
          <Input
            id="email"
            type="email"
            placeholder={t('login.email')}
            icon={Mail}
            value={email}
            onChange={e => setEmail(e.target.value)}
            required
            autoComplete="email"
            autoFocus
            {...v.fieldProps('email')}
          />
        </div>

        <div className="flex flex-col gap-1.5">
          <label htmlFor="password" className="text-xs font-medium text-text-secondary">
            {t('login.password')}
          </label>
          <Input
            id="password"
            type="password"
            placeholder={t('login.password')}
            icon={Lock}
            value={password}
            onChange={e => setPassword(e.target.value)}
            required
            autoComplete={isRegister ? 'new-password' : 'current-password'}
            error={v.errorFor('password')}
            aria-describedby={v.errorFor('password') ? undefined : isRegister ? 'login-password-rules' : undefined}
            onBlur={() => v.touch('password')}
          />
          {isRegister && <PasswordRuleList id="login-password-rules" check={check} />}
        </div>

        {isRegister && (
          <div className="flex flex-col gap-1.5">
            <label htmlFor="confirm-password" className="text-xs font-medium text-text-secondary">
              {t('login.confirmPassword')}
            </label>
            <Input
              id="confirm-password"
              type="password"
              placeholder={t('login.confirmPassword')}
              icon={Lock}
              value={confirmPassword}
              onChange={e => setConfirmPassword(e.target.value)}
              required
              autoComplete="new-password"
              {...v.fieldProps('confirmPassword')}
            />
          </div>
        )}

        <label className="flex items-center gap-2 text-sm text-text-secondary select-none cursor-pointer">
          <input
            type="checkbox"
            checked={remember}
            onChange={e => setRemember(e.target.checked)}
            className="accent-brand-500 w-3.5 h-3.5"
          />
          {t('login.rememberMe')}
        </label>

        {!isRegister && (
          <button
            type="button"
            className="text-sm text-brand-500 hover:underline font-medium self-start -mt-2"
            onClick={() => {
              clearError()
              setFormError(null)
              setForgot(true)
            }}
          >
            {t('login.forgotPassword')}
          </button>
        )}

        <Button
          type="submit"
          variant="primary"
          fullWidth
          loading={isLoading}
          icon={isRegister ? UserPlus : LogIn}
          disabled={isLoading || !canSubmit}
        >
          {isRegister ? t('login.submitRegister') : t('login.submitSignIn')}
        </Button>

        {sso?.enabled && (
          <>
            <div className="flex items-center gap-3 text-xs text-text-tertiary">
              <div className="flex-1 h-px bg-border-default" />
              {t('login.orDivider')}
              <div className="flex-1 h-px bg-border-default" />
            </div>
            <Button
              type="button"
              variant="secondary"
              fullWidth
              icon={KeyRound}
              disabled={isLoading}
              onClick={handleSSOStart}
            >
              {t('login.continueWith', {provider: sso.provider || 'SSO'})}
            </Button>
          </>
        )}

        {!signUpDisabled && (
          <div className="text-center text-sm text-text-tertiary">
            {isRegister ? t('login.hasAccount') : t('login.noAccount')}{' '}
            <button
              type="button"
              className="text-brand-500 hover:underline font-medium"
              onClick={() => {
                setIsRegister(!isRegister)
                clearError()
              }}
            >
              {isRegister ? t('login.switchToSignIn') : t('login.switchToRegister')}
            </button>
          </div>
        )}
      </form>
    </div>
  )
}
