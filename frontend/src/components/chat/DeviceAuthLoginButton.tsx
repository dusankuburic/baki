import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import {Check, KeyRound, Loader2} from 'lucide-react'
import {useState, useEffect, useRef, useCallback} from 'react'
import {createAdapter} from '@/platform/adapters'
import {logger} from '@/lib/logger'
import type {DeviceAuthResponse, GitHubAuthResult, GitHubUser} from '@/types'
import {PROVIDER_GLYPHS, hasProviderGlyph} from './providerGlyphs'

export const DEVICE_BUTTON_CLASS: Record<string, string> = {
  'github-models': 'bg-[#24292e] hover:bg-[#2f363d]',
  copilot: 'bg-[#6e40c9] hover:bg-[#5a32a3]',
}

export interface DeviceAuthProvider {
  name: string
  label: string
  buttonClassName: string
  icon?: string
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

function glyphOf(icon: string | undefined): string {
  if (!icon) return 'github'
  return hasProviderGlyph(icon) ? icon : 'key'
}

function DeviceGlyph({name}: {name: string}) {
  const Icon = hasProviderGlyph(name) ? PROVIDER_GLYPHS[name]! : KeyRound
  return <Icon size={16} />
}

export default function DeviceAuthLoginButton({provider, onAuthComplete}: Props) {
  const {t} = useTranslation('chat')
  const [state, setState] = useState<State>('unconfigured')
  const [userCode, setUserCode] = useState('')
  const [verificationURI, setVerificationURI] = useState('')
  const [username, setUsername] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [starting, setStarting] = useState(false)
  const attemptRef = useRef(0)
  const startingRef = useRef(false)
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => {
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
            try {
              const user = await provider.getUser()
              if (user?.login) setUsername(user.login)
            } catch (e) {
              logger.warn(`Fetch ${provider.name} user failed:`, e)
            }
            onAuthComplete?.()
            return
          }
          if (result.status === 'error') {
            setState('error')
            setErrorMsg(result.error || t('deviceAuth.authFailed'))
            return
          }
        } catch (e) {
          // GitHub granted the token but the server could not store it: the device code is spent, so polling on
          // would only relay GitHub's error for it. Stop with the storage failure instead of completing the sign-in.
          const code = getErrorCode(e)
          if (code === 'SECRET_STORAGE_UNAVAILABLE' || code === 'DEVICE_TOKEN_NOT_STORED') {
            if (attempt !== attemptRef.current) return
            setState('error')
            setErrorMsg(
              t(code === 'SECRET_STORAGE_UNAVAILABLE' ? 'deviceAuth.secretStorageUnavailable' : 'deviceAuth.tokenNotStored'),
            )
            return
          }
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

  const glyph = glyphOf(provider.icon)

  switch (state) {
    case 'unconfigured':
      return (
        <button
          className={`flex items-center gap-2 px-4 py-2 rounded-lg text-white text-sm transition-colors disabled:opacity-60 ${provider.buttonClassName}`}
          onClick={startAuth}
          disabled={starting}
          data-glyph={glyph}
        >
          {starting ? <Loader2 size={16} className="animate-spin" /> : <DeviceGlyph name={glyph} />}
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
