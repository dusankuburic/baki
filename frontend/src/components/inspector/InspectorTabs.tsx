import {useEffect} from 'react'
import {Info, Sparkles, AlertTriangle, BarChart3, Users, History} from 'lucide-react'
import type {LucideIcon} from 'lucide-react'
import {Tabs} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'

type Tab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing' | 'history'

// Sharing and History are cloud features (collaborators, stored versions):
// on desktop Sharing had no user to share as and History's Save Snapshot
// answered 503, so both tabs are dropped there rather than shown broken.
export const CLOUD_ONLY_TABS: readonly Tab[] = ['sharing', 'history']

const allTabs: {value: Tab; label: string; icon: LucideIcon}[] = [
  {value: 'details', label: 'Details', icon: Info},
  {value: 'ai', label: 'AI', icon: Sparkles},
  {value: 'findings', label: 'Findings', icon: AlertTriangle},
  {value: 'metrics', label: 'Metrics', icon: BarChart3},
  {value: 'sharing', label: 'Sharing', icon: Users},
  {value: 'history', label: 'History', icon: History},
]

export function visibleInspectorTabs() {
  return isTauri() ? allTabs.filter(t => !CLOUD_ONLY_TABS.includes(t.value)) : allTabs
}

export default function InspectorTabs() {
  const tab = useUIStore(s => s.inspectorTab)
  const setTab = useUIStore(s => s.setInspectorTab)
  const tabs = visibleInspectorTabs()

  // A stale selection (e.g. a command that targets a hidden tab) would leave
  // the tab strip with nothing active and an empty panel; snap back to Details.
  const hidden = !tabs.some(t => t.value === tab)
  useEffect(() => {
    if (hidden) setTab('details')
  }, [hidden, setTab])

  return (
    <Tabs
      items={tabs}
      value={tab}
      onChange={setTab}
      aria-label="Inspector sections"
      panelIdPrefix="inspector-panel"
      className="h-11 px-2 border-b border-border-default bg-surface-1"
    />
  )
}
