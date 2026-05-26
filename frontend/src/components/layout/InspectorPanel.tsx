import {InspectorTabs, DetailsTab} from '@/components/inspector'
import AITab from '@/components/chat/AITab'
import {FindingsTab} from '@/components/findings'
import {useUIStore} from '@/stores/uiStore'

export default function InspectorPanel() {
    const tab = useUIStore(s => s.inspectorTab)

    return (
        <div className="flex flex-col h-full bg-surface-1">
            <InspectorTabs />
            <div className="flex-1 overflow-hidden flex flex-col">
                {tab === 'details' && <DetailsTab />}
                {tab === 'ai' && <AITab />}
                {tab === 'findings' && <FindingsTab />}
            </div>
        </div>
    )
}
