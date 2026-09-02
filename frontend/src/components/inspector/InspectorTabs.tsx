import {Info, Sparkles, AlertTriangle, BarChart3, Users, History} from 'lucide-react'
import type {LucideIcon} from 'lucide-react'
import {Tabs} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'

type Tab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing' | 'history'

const tabs: {value: Tab; label: string; icon: LucideIcon}[] = [
  {value: 'details', label: 'Details', icon: Info},
  {value: 'ai', label: 'AI', icon: Sparkles},
  {value: 'findings', label: 'Findings', icon: AlertTriangle},
  {value: 'metrics', label: 'Metrics', icon: BarChart3},
  {value: 'sharing', label: 'Sharing', icon: Users},
  {value: 'history', label: 'History', icon: History},
]

export default function InspectorTabs() {
  const tab = useUIStore(s => s.inspectorTab)
  const setTab = useUIStore(s => s.setInspectorTab)

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
