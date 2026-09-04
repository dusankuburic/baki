import {useTranslation} from 'react-i18next'
import {lazy, Suspense, useState} from 'react'
import {InspectorTabs, DetailsTab} from '@/components/inspector'
import {FindingsTab} from '@/components/findings'
import {SharingTab} from '@/components/inspector/SharingTab'
import {HistoryTab} from '@/components/inspector/HistoryTab'
import {useUIStore} from '@/stores/uiStore'
import {Spinner, ErrorBoundary} from '@/components/shared'
import ResizableChatPanel from '@/components/chat/ResizableChatPanel'

// AITab transitively pulls react-markdown + prism-react-renderer; lazy
// loading keeps that chat-only weight out of the entry chunk.
const AITab = lazy(() => import('@/components/chat/AITab'))

// MetricsTab transitively pulls recharts (~100KB); lazy loading keeps the
// charting library out of the entry chunk until the user opens the Metrics tab.
const MetricsTab = lazy(() => import('@/components/inspector/MetricsTab'))

export default function InspectorPanel() {
  const {t} = useTranslation('shell')
  const tab = useUIStore(s => s.inspectorTab)
  // Once the AI tab has been visited, keep it mounted (hidden when inactive)
  // so switching tabs doesn't cancel in-flight streams or lose chat state.
  const [aiVisited, setAiVisited] = useState(false)
  // Monotonic latch, flipped during render: an effect would paint one frame with
  // the AI tab still unmounted before latching.
  if (tab === 'ai' && !aiVisited) setAiVisited(true)

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
          <div className={tab === 'ai' ? 'flex flex-col flex-1 min-h-0 overflow-hidden' : 'hidden'}>
            <ResizableChatPanel>
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
            </ResizableChatPanel>
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
        {tab === 'sharing' && (
          <ErrorBoundary fallbackMessage="Sharing tab error">
            <SharingTab />
          </ErrorBoundary>
        )}
        {tab === 'history' && (
          <ErrorBoundary fallbackMessage="History tab error">
            <HistoryTab />
          </ErrorBoundary>
        )}
      </div>
    </div>
  )
}
