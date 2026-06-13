import { useEffect, useState } from 'react'
import { Mail, Lock, LogIn, UserPlus, KeyRound } from 'lucide-react'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import { useAuthStore } from '@/stores/authStore'
import { authApi, type SSOInfo } from '@/api/auth'
import { getBackendConfig } from '@/api/client'

interface LoginFormProps {
  onSuccess?: () => void
}

// readSSOHash pulls ssoTicket / ssoError out of the URL fragment placed there
// by the OIDC callback redirect, and strips it from the address bar so the
// single-use ticket never lingers in history.
function readSSOHash(): { ticket?: string; error?: string } {
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

export default function LoginForm({ onSuccess }: LoginFormProps) {
  const [isRegister, setIsRegister] = useState(false)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [remember, setRemember] = useState(false)
  const [sso, setSso] = useState<SSOInfo | null>(null)
  const [ssoError, setSsoError] = useState<string | null>(null)
  const login = useAuthStore(s => s.login)
  const register = useAuthStore(s => s.register)
  const loginWithSSOTicket = useAuthStore(s => s.loginWithSSOTicket)
  const isLoading = useAuthStore(s => s.isLoading)
  const error = useAuthStore(s => s.error)
  const clearError = useAuthStore(s => s.clearError)

  useEffect(() => {
    authApi.ssoInfo().then(setSso).catch(() => setSso(null))

    const { ticket, error: hashError } = readSSOHash()
    if (hashError) setSsoError(hashError)
    if (ticket) {
      loginWithSSOTicket(ticket).then(() => onSuccess?.()).catch(() => {
        // error message lands in the store's `error` field
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleSSOStart() {
    clearError()
    setSsoError(null)
    const cfg = await getBackendConfig()
    window.location.href = `${cfg.apiUrl}/api/auth/sso/start`
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    clearError()

    if (isRegister && password !== confirmPassword) {
      alert("Passwords do not match")
      return
    }

    try {
      if (isRegister) {
        await register({ email, password }, remember)
      } else {
        await login({ email, password }, remember)
      }
      onSuccess?.()
    } catch {
      // error is set in the store
    }
  }

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-1">
      <form
        onSubmit={handleSubmit}
        className="w-full max-w-sm p-8 bg-surface-2 rounded-xl border border-border-default shadow-lg flex flex-col gap-5"
      >
        <div className="flex flex-col gap-1 mb-2">
          <h1 className="text-xl font-semibold text-text-primary">{isRegister ? 'Create Account' : 'Sign in'}</h1>
          <p className="text-sm text-text-muted">
            {isRegister ? 'Join PAD Analyzer today' : 'Enter your credentials to continue'}
          </p>
        </div>

        {(error || ssoError) && (
          <div className="px-3 py-2 bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-sm text-semantic-error">
            {error || ssoError}
          </div>
        )}

        <Input
          id="email"
          type="email"
          placeholder="Email"
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
          placeholder="Password"
          icon={Lock}
          value={password}
          onChange={e => setPassword(e.target.value)}
          required
          autoComplete={isRegister ? "new-password" : "current-password"}
        />

        {isRegister && (
          <Input
            id="confirm-password"
            type="password"
            placeholder="Confirm Password"
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

        <Button
          type="submit"
          variant="primary"
          fullWidth
          loading={isLoading}
          icon={isRegister ? UserPlus : LogIn}
          disabled={isLoading || !email || !password || (isRegister && !confirmPassword)}
        >
          {isRegister ? 'Sign up' : 'Sign in'}
        </Button>

        {sso?.enabled && (
          <>
            <div className="flex items-center gap-3 text-xs text-text-muted">
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

        <div className="text-center text-sm text-text-muted">
          {isRegister ? 'Already have an account?' : "Don't have an account?"}{' '}
          <button
            type="button"
            className="text-brand-500 hover:underline font-medium"
            onClick={() => {
              setIsRegister(!isRegister)
              clearError()
            }}
          >
            {isRegister ? 'Sign in' : 'Sign up'}
          </button>
        </div>
      </form>
    </div>
  )
}
