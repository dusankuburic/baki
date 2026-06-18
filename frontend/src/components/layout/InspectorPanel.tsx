import {lazy, Suspense, useState, useEffect} from 'react'
import {InspectorTabs, DetailsTab, MetricsTab} from '@/components/inspector'
import {FindingsTab} from '@/components/findings'
import {SharingTab} from '@/components/inspector/SharingTab'
import {HistoryTab} from '@/components/inspector/HistoryTab'
import {useUIStore} from '@/stores/uiStore'
import {Spinner, ErrorBoundary} from '@/components/shared'
import ResizableChatPanel from '@/components/chat/ResizableChatPanel'

// AITab transitively pulls react-markdown + react-syntax-highlighter; lazy
// loading keeps that chat-only weight out of the entry chunk.
const AITab = lazy(() => import('@/components/chat/AITab'))

export default function InspectorPanel() {
    const tab = useUIStore(s => s.inspectorTab)
    // Once the AI tab has been visited, keep it mounted (hidden when inactive)
    // so switching tabs doesn't cancel in-flight streams or lose chat state.
    const [aiVisited, setAiVisited] = useState(false)
    useEffect(() => {
        if (tab === 'ai') setAiVisited(true)
    }, [tab])

    return (
        <div className="flex flex-col h-full bg-surface-1">
            <InspectorTabs />
            <div className="flex-1 min-h-0 overflow-hidden flex flex-col">
                {tab === 'details' && <DetailsTab />}
                {(tab === 'ai' || aiVisited) && (
                    <div className={tab === 'ai' ? 'flex flex-col flex-1 min-h-0 overflow-hidden' : 'hidden'}>
                        <ResizableChatPanel>
                            <ErrorBoundary>
                            <Suspense fallback={<div className="flex-1 flex items-center justify-center"><Spinner /></div>}>
                                <AITab />
                            </Suspense>
                            </ErrorBoundary>
                        </ResizableChatPanel>
                    </div>
                )}
                {tab === 'findings' && <FindingsTab />}
                {tab === 'metrics' && <MetricsTab />}
                {tab === 'sharing' && <SharingTab />}
                {tab === 'history' && <HistoryTab />}
            </div>
        </div>
    )
}
