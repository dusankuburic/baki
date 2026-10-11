import {useTranslation} from 'react-i18next'
import {lazy, Suspense, useState} from 'react'
import {InspectorTabs, DetailsTab} from '@/components/inspector'
import {FindingsTab} from '@/components/findings'
import {SharingTab} from '@/components/inspector/SharingTab'
import {HistoryTab} from '@/components/inspector/HistoryTab'
import {useUIStore} from '@/stores/uiStore'
import {Spinner, ErrorBoundary} from '@/components/shared'
import ChatPanelShell from '@/components/chat/ChatPanelShell'
import {isTauri} from '@/platform/guards'

const AITab = lazy(() => import('@/components/chat/AITab'))

const MetricsTab = lazy(() => import('@/components/inspector/MetricsTab'))

export default function InspectorPanel() {
  const {t} = useTranslation('shell')
  const tab = useUIStore(s => s.inspectorTab)
  const chatFocus = useUIStore(s => s.chatFocus)
  const [aiVisited, setAiVisited] = useState(false)
  if ((tab === 'ai' || chatFocus) && !aiVisited) setAiVisited(true)
  const cloudTabs = !isTauri()

  return (
    <div className="flex flex-col h-full bg-surface-1">
      <InspectorTabs />
      <div
        id={`inspector-panel-${tab}`}
        role="tabpanel"
        aria-label={t('inspector.panelAria')}
        className="flex-1 min-h-0 overflow-hidden flex flex-col"
      >
        {tab === 'details' && (
          <ErrorBoundary fallbackMessage="Details tab error">
            <DetailsTab />
          </ErrorBoundary>
        )}
        {(tab === 'ai' || aiVisited) && (
          <div
            className={
              tab === 'ai' ? 'flex flex-col flex-1 min-h-0 overflow-hidden' : chatFocus ? 'contents' : 'hidden'
            }
          >
            <ChatPanelShell>
              <ErrorBoundary fallbackMessage="AI assistant error">
                <Suspense
                  fallback={
                    <div className="flex-1 flex items-center justify-center">
                      <Spinner />
                    </div>
                  }
                >
                  <AITab />
                </Suspense>
              </ErrorBoundary>
            </ChatPanelShell>
          </div>
        )}
        {tab === 'findings' && (
          <ErrorBoundary fallbackMessage="Findings tab error">
            <FindingsTab />
          </ErrorBoundary>
        )}
        {tab === 'metrics' && (
          <ErrorBoundary fallbackMessage="Metrics tab error">
            <Suspense
              fallback={
                <div className="flex-1 flex items-center justify-center">
                  <Spinner />
                </div>
              }
            >
              <MetricsTab />
            </Suspense>
          </ErrorBoundary>
        )}
        {cloudTabs && tab === 'sharing' && (
          <ErrorBoundary fallbackMessage="Sharing tab error">
            <SharingTab />
          </ErrorBoundary>
        )}
        {cloudTabs && tab === 'history' && (
          <ErrorBoundary fallbackMessage="History tab error">
            <HistoryTab />
          </ErrorBoundary>
        )}
      </div>
    </div>
  )
}
