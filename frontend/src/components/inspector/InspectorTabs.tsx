import {useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import {Info, Sparkles, AlertTriangle, BarChart3, Users, History} from 'lucide-react'
import type {LucideIcon} from 'lucide-react'
import {Tabs} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'

type Tab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing' | 'history'

export const CLOUD_ONLY_TABS: readonly Tab[] = ['sharing', 'history']

const allTabs: {value: Tab; icon: LucideIcon}[] = [
  {value: 'details', icon: Info},
  {value: 'ai', icon: Sparkles},
  {value: 'findings', icon: AlertTriangle},
  {value: 'metrics', icon: BarChart3},
  {value: 'sharing', icon: Users},
  {value: 'history', icon: History},
]

const TAB_LABEL_KEYS = {
  details: 'tabs.details',
  ai: 'tabs.ai',
  findings: 'tabs.findings',
  metrics: 'tabs.metrics',
  sharing: 'tabs.sharing',
  history: 'tabs.history',
} as const

export function visibleInspectorTabs() {
  return isTauri() ? allTabs.filter(t => !CLOUD_ONLY_TABS.includes(t.value)) : allTabs
}

export default function InspectorTabs() {
  const {t} = useTranslation('inspector')
  const tab = useUIStore(s => s.inspectorTab)
  const setTab = useUIStore(s => s.setInspectorTab)
  const tabs = visibleInspectorTabs().map(tabDef => ({...tabDef, label: t(TAB_LABEL_KEYS[tabDef.value])}))

  const hidden = !tabs.some(t => t.value === tab)
  useEffect(() => {
    if (hidden) setTab('details')
  }, [hidden, setTab])

  return (
    <Tabs
      items={tabs}
      value={tab}
      onChange={setTab}
      aria-label={t('tabs.sectionsAria')}
      panelIdPrefix="inspector-panel"
      className="h-11 px-2 border-b border-border-default bg-surface-1"
    />
  )
}
