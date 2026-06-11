import {lazy, Suspense} from 'react'
import {InspectorTabs, DetailsTab, MetricsTab} from '@/components/inspector'
import {FindingsTab} from '@/components/findings'
import {SharingTab} from '@/components/inspector/SharingTab'
import {HistoryTab} from '@/components/inspector/HistoryTab'
import {useUIStore} from '@/stores/uiStore'
import {Spinner} from '@/components/shared'
import ResizableChatPanel from '@/components/chat/ResizableChatPanel'

// AITab transitively pulls react-markdown + react-syntax-highlighter; lazy
// loading keeps that chat-only weight out of the entry chunk.
const AITab = lazy(() => import('@/components/chat/AITab'))

export default function InspectorPanel() {
    const tab = useUIStore(s => s.inspectorTab)

    return (
        <div className="flex flex-col h-full bg-surface-1">
            <InspectorTabs />
            <div className="flex-1 min-h-0 overflow-hidden flex flex-col">
                {tab === 'details' && <DetailsTab />}
                {tab === 'ai' && (
                    <ResizableChatPanel>
                        <Suspense fallback={<div className="flex-1 flex items-center justify-center"><Spinner /></div>}>
                            <AITab />
                        </Suspense>
                    </ResizableChatPanel>
                )}
                {tab === 'findings' && <FindingsTab />}
                {tab === 'metrics' && <MetricsTab />}
                {tab === 'sharing' && <SharingTab />}
                {tab === 'history' && <HistoryTab />}
            </div>
        </div>
    )
}
