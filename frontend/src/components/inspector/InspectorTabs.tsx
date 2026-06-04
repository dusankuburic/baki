import clsx from 'clsx'
import {Info, Sparkles, AlertTriangle, BarChart3, Users} from 'lucide-react'
import type {LucideIcon} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'

type Tab = 'details' | 'ai' | 'findings' | 'metrics' | 'sharing'

const tabs: {value: Tab; label: string; icon: LucideIcon}[] = [
    {value: 'details',  label: 'Details',  icon: Info},
    {value: 'ai',       label: 'AI',       icon: Sparkles},
    {value: 'findings', label: 'Findings', icon: AlertTriangle},
    {value: 'metrics',  label: 'Metrics',  icon: BarChart3},
    {value: 'sharing',  label: 'Sharing',  icon: Users},
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
                        className={clsx(
                            'flex-1 flex items-center justify-center gap-1.5 h-7 px-2 text-xs font-medium',
                            'rounded-md transition-colors duration-fast',
                            tab === t.value
                                ? 'bg-surface-3 text-text-primary shadow-xs'
                                : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-2'
                        )}
                        onClick={() => setTab(t.value)}
                    >
                        <Icon size={13} />
                        {t.label}
                    </button>
                )
            })}
        </div>
    )
}
