import {useEffect, useState} from 'react'
import {Mail, Lock, LogIn, UserPlus, KeyRound, Send} from 'lucide-react'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {useAuthStore} from '@/stores/authStore'
import {authApi, type SSOInfo} from '@/api/auth'
import {getBackendConfig} from '@/api/client'
import {useTranslation} from 'react-i18next'

interface LoginFormProps {
  onSuccess?: () => void
}

// readSSOHash pulls ssoTicket / ssoError out of the URL fragment placed there
// by the OIDC callback redirect, and strips it from the address bar so the
// single-use ticket never lingers in history.
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
  const [ssoError, setSsoError] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  // Forgot-password sub-flow: a small email-only form reachable from the login
  // view. `forgotSent` shows a deliberately generic confirmation (the API never
  // reveals whether the address exists).
  const [forgot, setForgot] = useState(false)
  const [forgotSent, setForgotSent] = useState(false)
  const [forgotLoading, setForgotLoading] = useState(false)
  const login = useAuthStore(s => s.login)
  const register = useAuthStore(s => s.register)
  const loginWithSSOTicket = useAuthStore(s => s.loginWithSSOTicket)
  const isLoading = useAuthStore(s => s.isLoading)
  const error = useAuthStore(s => s.error)
  const clearError = useAuthStore(s => s.clearError)

  useEffect(() => {
    // Cancellation guard: these promises can resolve after unmount (route
    // change, test teardown) — a setState then hits a dead component.
    let cancelled = false
    authApi
      .ssoInfo()
      .then(info => {
        if (!cancelled) setSso(info)
      })
      .catch(() => {
        if (!cancelled) setSso(null)
      })

    const {ticket, error: hashError} = readSSOHash()
    // One-shot parse of the URL fragment on mount: an external input, not derived state.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (hashError) setSsoError(hashError)
    if (ticket) {
      loginWithSSOTicket(ticket)
        .then(() => {
          if (!cancelled) onSuccess?.()
        })
        .catch(() => {
          // error message lands in the store's `error` field
        })
    }
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleSSOStart() {
    clearError()
    setSsoError(null)
    const cfg = await getBackendConfig()
    window.location.href = `${cfg.apiUrl}/api/auth/sso/start`
  }

  async function handleForgotSubmit(e: React.FormEvent) {
    e.preventDefault()
    setFormError(null)
    setForgotLoading(true)
    try {
      await authApi.forgotPassword(email)
      setForgotSent(true)
    } catch {
      // Even on error we show the generic confirmation so the endpoint can't be
      // used to probe for accounts.
      setForgotSent(true)
    } finally {
      setForgotLoading(false)
    }
  }

  function backToLogin() {
    setForgot(false)
    setForgotSent(false)
    setFormError(null)
    clearError()
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    clearError()
    setFormError(null)

    if (isRegister && password !== confirmPassword) {
      setFormError(t('login.passwordsDoNotMatch'))
      return
    }

    try {
      if (isRegister) {
        await register({email, password}, remember)
      } else {
        await login({email, password}, remember)
      }
      onSuccess?.()
    } catch {
      // error is set in the store
    }
  }

  if (forgot) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-1">
        <form
          onSubmit={handleForgotSubmit}
          className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5"
        >
          <div className="flex flex-col gap-1 mb-2">
            <h1 className="text-xl font-semibold text-text-primary">Reset your password</h1>
            <p className="text-sm text-text-tertiary">{forgotSent ? t('login.resetSent') : t('login.resetPrompt')}</p>
          </div>

          {!forgotSent && (
            <>
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
              <Button
                type="submit"
                variant="primary"
                fullWidth
                loading={forgotLoading}
                icon={Send}
                disabled={forgotLoading || !email}
              >
                Send reset link
              </Button>
            </>
          )}

          <div className="text-center text-sm text-text-tertiary">
            <button type="button" className="text-brand-500 hover:underline font-medium" onClick={backToLogin}>
              Back to sign in
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

        {(error || ssoError || formError) && (
          <div
            role="alert"
            className="px-3 py-2 bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-sm text-semantic-error"
          >
            {error || ssoError || formError}
          </div>
        )}

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
        />

        <Input
          id="password"
          type="password"
          placeholder={t('login.password')}
          icon={Lock}
          value={password}
          onChange={e => setPassword(e.target.value)}
          required
          autoComplete={isRegister ? 'new-password' : 'current-password'}
        />

        {isRegister && (
          <Input
            id="confirm-password"
            type="password"
            placeholder={t('login.confirmPassword')}
            icon={Lock}
            value={confirmPassword}
            onChange={e => setConfirmPassword(e.target.value)}
            required
            autoComplete="new-password"
          />
        )}

        <label className="flex items-center gap-2 text-sm text-text-secondary select-none cursor-pointer">
          <input
            type="checkbox"
            checked={remember}
            onChange={e => setRemember(e.target.checked)}
            className="accent-brand-500 w-3.5 h-3.5"
          />
          Keep me signed in on this device
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
            Forgot password?
          </button>
        )}

        <Button
          type="submit"
          variant="primary"
          fullWidth
          loading={isLoading}
          icon={isRegister ? UserPlus : LogIn}
          disabled={isLoading || !email || !password || (isRegister && !confirmPassword)}
        >
          {isRegister ? t('login.submitRegister') : t('login.submitSignIn')}
        </Button>

        {sso?.enabled && (
          <>
            <div className="flex items-center gap-3 text-xs text-text-tertiary">
              <div className="flex-1 h-px bg-border-default" />
              or
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
              Continue with {sso.provider || 'SSO'}
            </Button>
          </>
        )}

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
      </form>
    </div>
  )
}
