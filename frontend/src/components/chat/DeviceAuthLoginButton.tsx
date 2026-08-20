import {Github, Check, Loader2} from 'lucide-react'
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
  const [state, setState] = useState<State>('unconfigured')
  const [userCode, setUserCode] = useState('')
  const [verificationURI, setVerificationURI] = useState('')
  const [username, setUsername] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [_deviceCode, setDeviceCode] = useState('')
  const pollingRef = useRef(false)
  const timeoutRef = useRef<ReturnType<typeof setTimeout>>()

  useEffect(() => {
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
      pollingRef.current = false
      clearTimeout(timeoutRef.current)
    }
  }, [provider])

  const startAuth = useCallback(async () => {
    try {
      const resp = await provider.startAuth()
      if (!resp) return
      setDeviceCode(resp.device_code)
      setUserCode(resp.user_code)
      setVerificationURI(resp.verification_uri)
      setState('authorizing')
      void createAdapter().openURL(resp.verification_uri)

      pollingRef.current = true
      const interval = (resp.interval || 5) * 1000

      const poll = async () => {
        if (!pollingRef.current) return
        try {
          const result = await provider.pollAuth(resp.device_code)
          if (result.status === 'success') {
            pollingRef.current = false
            setState('configured')
            const user = await provider.getUser()
            if (user?.login) setUsername(user.login)
            onAuthComplete?.()
            return
          }
          if (result.status === 'error') {
            pollingRef.current = false
            setState('error')
            setErrorMsg(result.error || 'Authentication failed')
            return
          }
        } catch {
          /* polling error — retry on next interval */
        }
        if (pollingRef.current) {
          timeoutRef.current = setTimeout(poll, interval)
        }
      }
      timeoutRef.current = setTimeout(poll, interval)
    } catch (e: unknown) {
      setState('error')
      setErrorMsg(e instanceof Error ? e.message : String(e) || 'Failed to start auth')
    }
  }, [provider, onAuthComplete])

  const cancel = useCallback(() => {
    pollingRef.current = false
    setState('unconfigured')
    setUserCode('')
    setDeviceCode('')
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
          className={`flex items-center gap-2 px-4 py-2 rounded-lg text-white text-sm transition-colors ${provider.buttonClassName}`}
          onClick={startAuth}
        >
          <Github size={16} />
          {provider.label}
        </button>
      )

    case 'authorizing':
      return (
        <div className="bg-surface-2 border border-border-default rounded-lg p-4 space-y-3">
          <p className="text-sm text-text-secondary">To authenticate, visit:</p>
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
          <p className="text-sm text-text-secondary">Enter code:</p>
          <p className="text-2xl font-mono font-bold text-text-primary tracking-wider">{userCode}</p>
          <div className="flex items-center gap-2 text-sm text-text-tertiary">
            <Loader2 size={14} className="animate-spin" />
            Waiting for authorization...
          </div>
          <button className="text-xs text-text-tertiary hover:text-text-secondary" onClick={cancel}>
            Cancel
          </button>
        </div>
      )

    case 'configured':
      return (
        <div className="flex items-center gap-2">
          <Check size={14} className="text-green-500" />
          <span className="text-sm text-text-secondary">Connected as @{username}</span>
          <button className="text-xs text-text-tertiary hover:text-text-secondary ml-2" onClick={disconnect}>
            Disconnect
          </button>
        </div>
      )

    case 'error':
      return (
        <div className="space-y-2">
          <p className="text-sm text-red-400">{errorMsg}</p>
          <button className="text-xs text-brand-400 hover:text-brand-300" onClick={startAuth}>
            Retry
          </button>
        </div>
      )
  }
}
