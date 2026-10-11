import {useEffect, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {AlertOctagon, RefreshCw} from 'lucide-react'
import {usePlatform} from '@/hooks/usePlatform'
import {logger} from '@/lib/logger'
import type {BackendEvent} from '@/platform/types'
import {getErrorMessage} from '@/lib/getErrorMessage'

type Phase = 'terminated' | 'error' | 'restarting'

export default function BackendCrashBanner() {
  const {t} = useTranslation('shell')
  const platform = usePlatform()
  const [phase, setPhase] = useState<Phase | null>(null)
  const [detail, setDetail] = useState('')

  useEffect(() => {
    return platform.onBackendEvent((event: BackendEvent) => {
      switch (event.kind) {
        case 'ready':
          setPhase(null)
          setDetail('')
          break
        case 'error':
          setPhase('error')
          setDetail(event.message)
          break
        case 'terminated':
          setPhase(p => (p === 'error' || p === 'restarting' ? p : 'terminated'))
          setDetail(event.message)
          break
      }
    })
  }, [platform])

  const handleRestart = async () => {
    setPhase('restarting')
    try {
      await platform.restartBackend()
      window.location.reload()
    } catch (err) {
      logger.error('restart_backend failed:', err)
      setPhase('error')
      setDetail(getErrorMessage(err))
    }
  }

  if (!phase) return null

  const title =
    phase === 'error'
      ? t('backendCrash.titleError')
      : phase === 'restarting'
        ? t('backendCrash.titleRestarting')
        : t('backendCrash.titleTerminated')

  return (
    <div
      className="fixed inset-0 z-modal bg-surface-overlay flex items-center justify-center p-4 print:hidden"
      role="alertdialog"
      aria-modal="true"
      aria-labelledby="backend-crash-title"
    >
      <div className="w-full max-w-md rounded-xl border border-semantic-error/40 bg-surface-1 shadow-xl p-5 flex flex-col gap-3">
        <div className="flex items-center gap-2 text-semantic-error">
          <AlertOctagon size={18} />
          <h2 id="backend-crash-title" className="text-sm font-semibold text-text-primary">
            {title}
          </h2>
        </div>
        <p className="text-xs text-text-secondary">{t('backendCrash.body')}</p>
        {detail && (
          <pre className="text-2xs text-text-tertiary whitespace-pre-wrap wrap-break-word max-h-24 overflow-auto">
            {detail}
          </pre>
        )}
        <div className="flex justify-end">
          <button
            onClick={handleRestart}
            disabled={phase === 'restarting'}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-brand-500 text-brand-foreground text-xs font-medium hover:bg-brand-600 disabled:opacity-50 transition-colors"
          >
            <RefreshCw size={12} className={phase === 'restarting' ? 'animate-spin' : ''} />
            {phase === 'restarting' ? t('backendCrash.restarting') : t('backendCrash.restart')}
          </button>
        </div>
      </div>
    </div>
  )
}
