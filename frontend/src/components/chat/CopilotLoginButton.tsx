import {Github, Check, Loader2} from 'lucide-react'
import {useState, useEffect, useRef, useCallback} from 'react'
import {providersApi} from '@/api'
import {open} from '@tauri-apps/plugin-shell'

interface Props {
  onAuthComplete?: () => void
}

type State = 'unconfigured' | 'authorizing' | 'configured' | 'error'

export default function CopilotLoginButton({onAuthComplete}: Props) {
  const [state, setState] = useState<State>('unconfigured')
  const [userCode, setUserCode] = useState('')
  const [verificationURI, setVerificationURI] = useState('')
  const [username, setUsername] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [_deviceCode, setDeviceCode] = useState('')
  const pollingRef = useRef(false)
  const timeoutRef = useRef<ReturnType<typeof setTimeout>>()

  useEffect(() => {
    providersApi.getCopilotUser()
      .then(user => {
        if (user?.login) {
          setUsername(user.login)
          setState('configured')
        }
      })
      .catch(() => {})
    return () => {
      pollingRef.current = false
      clearTimeout(timeoutRef.current)
    }
  }, [])

  const startAuth = useCallback(async () => {
    try {
      const resp = await providersApi.startCopilotAuth()
      if (!resp) return
      setDeviceCode(resp.device_code)
      setUserCode(resp.user_code)
      setVerificationURI(resp.verification_uri)
      setState('authorizing')
      open(resp.verification_uri)

      pollingRef.current = true
      const interval = (resp.interval || 5) * 1000

      const poll = async () => {
        if (!pollingRef.current) return
        try {
          const result = await providersApi.pollCopilotAuth(resp.device_code)
          if (result.status === 'success') {
            pollingRef.current = false
            setState('configured')
            const user = await providersApi.getCopilotUser()
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
        } catch (_e) { /* polling error — retry on next interval */ }
        if (pollingRef.current) {
          timeoutRef.current = setTimeout(poll, interval)
        }
      }
      timeoutRef.current = setTimeout(poll, interval)
    } catch (e: any) {
      setState('error')
      setErrorMsg(e?.message || 'Failed to start auth')
    }
  }, [onAuthComplete])

  const cancel = useCallback(() => {
    pollingRef.current = false
    setState('unconfigured')
    setUserCode('')
    setDeviceCode('')
  }, [])

  const disconnect = useCallback(async () => {
    try {
      await providersApi.revokeCopilotAuth()
    } catch (e) { console.error('Revoke Copilot auth failed:', e) }
    setState('unconfigured')
    setUsername('')
  }, [])

  switch (state) {
    case 'unconfigured':
      return (
        <button
          className="flex items-center gap-2 px-4 py-2 rounded-lg bg-[#6e40c9] hover:bg-[#5a32a3] text-white text-sm transition-colors"
          onClick={startAuth}
        >
          <Github size={16} />
          Sign in with GitHub
        </button>
      )

    case 'authorizing':
      return (
        <div className="bg-surface-2 border border-border-default rounded-lg p-4 space-y-3">
          <p className="text-sm text-text-secondary">
            To authenticate, visit:
          </p>
          <a
            href={verificationURI}
            className="text-sm text-brand-400 underline break-all"
            onClick={e => {
              e.preventDefault()
              open(verificationURI)
            }}
          >
            {verificationURI}
          </a>
          <p className="text-sm text-text-secondary">
            Enter code:
          </p>
          <p className="text-2xl font-mono font-bold text-text-primary tracking-wider">
            {userCode}
          </p>
          <div className="flex items-center gap-2 text-sm text-text-tertiary">
            <Loader2 size={14} className="animate-spin" />
            Waiting for authorization...
          </div>
          <button
            className="text-xs text-text-tertiary hover:text-text-secondary"
            onClick={cancel}
          >
            Cancel
          </button>
        </div>
      )

    case 'configured':
      return (
        <div className="flex items-center gap-2">
          <Check size={14} className="text-green-500" />
          <span className="text-sm text-text-secondary">
            Connected as @{username}
          </span>
          <button
            className="text-xs text-text-tertiary hover:text-text-secondary ml-2"
            onClick={disconnect}
          >
            Disconnect
          </button>
        </div>
      )

    case 'error':
      return (
        <div className="space-y-2">
          <p className="text-sm text-red-400">{errorMsg}</p>
          <button
            className="text-xs text-brand-400 hover:text-brand-300"
            onClick={startAuth}
          >
            Retry
          </button>
        </div>
      )
  }
}
