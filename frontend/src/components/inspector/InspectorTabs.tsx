import clsx from 'clsx'
import {Info, Sparkles, AlertTriangle, BarChart3, Users, History} from 'lucide-react'
import type {LucideIcon} from 'lucide-react'
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
    <div className="flex items-center h-11 px-2 gap-1 border-b border-border-default bg-surface-1">
      {tabs.map(t => {
        const Icon = t.icon
        return (
          <button
            key={t.value}
            title={t.label}
            className={clsx(
              'flex-1 min-w-0 overflow-hidden flex items-center justify-center gap-1.5 h-7 px-2 text-xs font-medium',
              'rounded-md transition-colors duration-fast',
              'focus-visible:ring-2 focus-visible:ring-brand-500/40 focus-visible:outline-none',
              tab === t.value
                ? 'bg-surface-3 text-text-primary shadow-xs'
                : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-2',
            )}
            onClick={() => setTab(t.value)}
          >
            <Icon size={13} className="flex-shrink-0" />
            <span className="truncate min-w-0">{t.label}</span>
          </button>
        )
      })}
    </div>
  )
}
