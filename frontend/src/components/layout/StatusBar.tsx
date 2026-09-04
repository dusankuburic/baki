import {useTranslation} from 'react-i18next'
import {useEffect, useState} from 'react'
import {Check, User, Shield, Radio} from 'lucide-react'
import {Spinner} from '@/components/shared'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAuthStore} from '@/stores/authStore'
import {useUIStore} from '@/stores/uiStore'
import {useSyncStore} from '@/stores/syncStore'
import {subscribeConnectionState, type EventConnectionState} from '@/api/client'

export default function StatusBar() {
  const {t} = useTranslation('shell')
  const document = useFlowStore(s => s.document)
  const isParsing = useFlowStore(s => s.isParsing)
  const parseProgress = useFlowStore(s => s.parseProgress)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const progress = useAnalysisStore(s => s.progress)
  const report = useAnalysisStore(s => (document ? s.reports.get(document.id) : undefined))
  const user = useAuthStore(s => s.user)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const pendingCount = useSyncStore(s => s.pendingCount)

  const [conn, setConn] = useState<EventConnectionState>('idle')
  useEffect(() => {
    return subscribeConnectionState(setConn)
  }, [])

  const blockCount = document?.metadata?.blockCount ?? 0
  const subflowCount = document?.metadata?.subflowCount ?? 0

  const findingTotal = report ? report.stats.errors + report.stats.warnings + report.stats.info : 0

  return (
    <div className="flex items-center h-6 px-3 border-t border-border-subtle bg-surface-1 text-xs text-text-tertiary tabular-nums print:hidden">
      <div className="flex items-center gap-1.5 flex-1">
        {document ? (
          <>
            <Check size={12} className="text-semantic-success" />
            <span>{t('status.parsed', {blocks: blockCount, subflows: subflowCount})}</span>
            {isAnalyzing && (
              <span className="flex items-center gap-1 ml-2">
                <Spinner size={10} />
                {t('status.analyzing', {
                  percent: progress.total > 0 ? Math.round((progress.current / progress.total) * 100) : 0,
                })}
              </span>
            )}
            {!isAnalyzing && report && (
              <span className="ml-2">
                {t('status.findingsSummary', {
                  total: findingTotal,
                  errors: report.stats.errors,
                  warnings: report.stats.warnings,
                })}
              </span>
            )}
          </>
        ) : isParsing ? (
          <span className="flex items-center gap-1">
            <Spinner size={10} />
            {t('status.parsing', {percent: parseProgress > 0 ? `${parseProgress}%` : ''})}
          </span>
        ) : (
          <span>{t('status.ready')}</span>
        )}
      </div>
      <div className="flex items-center gap-4">
        {conn === 'open' && (
          <span className="flex items-center gap-1 text-semantic-success" title={t('status.liveTitle')}>
            <Radio size={10} />
            <span>{t('status.live')}</span>
          </span>
        )}
        {(conn === 'reconnecting' || conn === 'connecting') && (
          <span className="flex items-center gap-1 text-semantic-warning" title={t('status.connectionTitle')}>
            <Spinner size={10} />
            {conn === 'reconnecting' ? t('status.reconnecting') : t('status.connecting')}
          </span>
        )}
        {pendingCount > 0 && (
          <span
            className="flex items-center gap-1 text-semantic-warning"
            title={t('status.pendingTitle', {count: pendingCount})}
          >
            <span>{t('status.unsaved', {count: pendingCount})}</span>
          </span>
        )}
        {user && (
          <>
            {user.role === 'admin' && (
              <button
                onClick={() => setMainPaneView('admin')}
                className="flex items-center gap-1 hover:text-text-secondary transition-colors"
                title={t('status.adminTitle')}
              >
                <Shield size={12} />
                <span>{t('status.admin')}</span>
              </button>
            )}
            <button
              onClick={() => setMainPaneView('profile')}
              className="flex items-center gap-1 hover:text-text-secondary transition-colors"
              title={t('status.profileTitle')}
            >
              <User size={12} />
              <span>{user.email}</span>
            </button>
          </>
        )}
        {document && <span>{document.name}</span>}
      </div>
    </div>
  )
}
