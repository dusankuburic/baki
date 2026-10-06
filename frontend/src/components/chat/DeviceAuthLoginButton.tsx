import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import {Check, Loader2} from 'lucide-react'
import {GithubMark} from '@/components/shared'
import {useState, useEffect, useRef, useCallback} from 'react'
import {createAdapter} from '@/platform/adapters'
import {logger} from '@/lib/logger'
import type {DeviceAuthResponse, GitHubAuthResult, GitHubUser} from '@/types'

/**
 * Provider-specific configuration for the OAuth device-flow login button.
 * GitHub and Copilot share the same device-flow UX and only differ in the
 * API calls they invoke, the button colour, and the provider label.
 */
export interface DeviceAuthProvider {
  /** Provider label used in log messages (e.g. "GitHub", "Copilot"). */
  name: string
  /** Button text shown in the unconfigured state. */
  label: string
  /** Tailwind classes for the sign-in button background. */
  buttonClassName: string
  getUser: () => Promise<GitHubUser | null>
  startAuth: () => Promise<DeviceAuthResponse | null>
  pollAuth: (deviceCode: string) => Promise<GitHubAuthResult>
  revokeAuth: () => Promise<void>
}

interface Props {
  provider: DeviceAuthProvider
  onAuthComplete?: () => void
}

type State = 'unconfigured' | 'authorizing' | 'configured' | 'error'

export default function DeviceAuthLoginButton({provider, onAuthComplete}: Props) {
  const {t} = useTranslation('chat')
  const [state, setState] = useState<State>('unconfigured')
  const [userCode, setUserCode] = useState('')
  const [verificationURI, setVerificationURI] = useState('')
  const [username, setUsername] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [starting, setStarting] = useState(false)
  // attemptRef identifies the current sign-in attempt; every await and timer
  // checks it, so a cancelled or superseded attempt can never touch state
  // again. A shared "polling" flag used to let Cancel → Sign in revive the old
  // attempt's poll loop, which then killed the new sign-in when its abandoned
  // code expired.
  const attemptRef = useRef(0)
  const startingRef = useRef(false)
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => {
    // The attempt counter (not a DOM node): bumping it on unmount/provider
    // change invalidates any sign-in still in flight.
    const attempts = attemptRef
    provider
      .getUser()
      .then(user => {
        if (user?.login) {
          setUsername(user.login)
          setState('configured')
        }
      })
      .catch(err => {
        logger.warn('Failed to check existing auth', err)
      })
    return () => {
      attempts.current++
      clearTimeout(timeoutRef.current)
    }
  }, [provider])

  const startAuth = useCallback(async () => {
    // One sign-in at a time: a double-click started two device flows and
    // opened two browser tabs.
    if (startingRef.current) return
    startingRef.current = true
    setStarting(true)
    clearTimeout(timeoutRef.current)
    const attempt = ++attemptRef.current
    try {
      const resp = await provider.startAuth()
      if (attempt !== attemptRef.current || !resp) return
      setUserCode(resp.user_code)
      setVerificationURI(resp.verification_uri)
      setState('authorizing')
      void createAdapter().openURL(resp.verification_uri)

      const interval = (resp.interval || 5) * 1000
      // The device code is only valid for expires_in; polling past it can only
      // fail, so stop and say so instead of spinning forever.
      const expiresAt = Date.now() + (resp.expires_in > 0 ? resp.expires_in * 1000 : 15 * 60 * 1000)

      const poll = async () => {
        if (attempt !== attemptRef.current) return
        if (Date.now() >= expiresAt) {
          setState('error')
          setErrorMsg(t('deviceAuth.expired'))
          return
        }
        try {
          const result = await provider.pollAuth(resp.device_code)
          if (attempt !== attemptRef.current) return
          if (result.status === 'success') {
            attemptRef.current++
            setState('configured')
            const user = await provider.getUser()
            if (user?.login) setUsername(user.login)
            onAuthComplete?.()
            return
          }
          if (result.status === 'error') {
            setState('error')
            setErrorMsg(result.error || t('deviceAuth.authFailed'))
            return
          }
        } catch {
          /* polling error — retry on next interval */
        }
        if (attempt === attemptRef.current) {
          timeoutRef.current = setTimeout(poll, interval)
        }
      }
      timeoutRef.current = setTimeout(poll, interval)
    } catch (e: unknown) {
      if (attempt !== attemptRef.current) return
      setState('error')
      setErrorMsg(getErrorMessage(e) || t('deviceAuth.startFailed'))
    } finally {
      startingRef.current = false
      setStarting(false)
    }
  }, [provider, onAuthComplete, t])

  const cancel = useCallback(() => {
    attemptRef.current++
    clearTimeout(timeoutRef.current)
    setState('unconfigured')
    setUserCode('')
  }, [])

  const disconnect = useCallback(async () => {
    try {
      await provider.revokeAuth()
    } catch (e) {
      logger.warn(`Revoke ${provider.name} auth failed:`, e)
    }
    setState('unconfigured')
    setUsername('')
  }, [provider])

  switch (state) {
    case 'unconfigured':
      return (
        <button
          className={`flex items-center gap-2 px-4 py-2 rounded-lg text-white text-sm transition-colors disabled:opacity-60 ${provider.buttonClassName}`}
          onClick={startAuth}
          disabled={starting}
        >
          {starting ? <Loader2 size={16} className="animate-spin" /> : <GithubMark size={16} />}
          {provider.label}
        </button>
      )

    case 'authorizing':
      return (
        <div className="bg-surface-2 border border-border-default rounded-lg p-4 space-y-3">
          <p className="text-sm text-text-secondary">{t('deviceAuth.visitPrompt')}</p>
          <a
            href={verificationURI}
            className="text-sm text-brand-400 underline break-all"
            onClick={e => {
              e.preventDefault()
              void createAdapter().openURL(verificationURI)
            }}
          >
            {verificationURI}
          </a>
          <p className="text-sm text-text-secondary">{t('deviceAuth.enterCode')}</p>
          <p className="text-2xl font-mono font-bold text-text-primary tracking-wider">{userCode}</p>
          <div className="flex items-center gap-2 text-sm text-text-tertiary">
            <Loader2 size={14} className="animate-spin" />
            {t('deviceAuth.waiting')}
          </div>
          <button className="text-xs text-text-tertiary hover:text-text-secondary" onClick={cancel}>
            {t('deviceAuth.cancel')}
          </button>
        </div>
      )

    case 'configured':
      return (
        <div className="flex items-center gap-2">
          <Check size={14} className="text-semantic-success" />
          <span className="text-sm text-text-secondary">{t('deviceAuth.connectedAs', {username})}</span>
          <button className="text-xs text-text-tertiary hover:text-text-secondary ml-2" onClick={disconnect}>
            {t('deviceAuth.disconnect')}
          </button>
        </div>
      )

    case 'error':
      return (
        <div className="space-y-2">
          <p className="text-sm text-semantic-error">{errorMsg}</p>
          <button
            className="text-xs text-brand-400 hover:text-brand-300 disabled:opacity-60"
            onClick={startAuth}
            disabled={starting}
          >
            {t('deviceAuth.retry')}
          </button>
        </div>
      )
  }
}
