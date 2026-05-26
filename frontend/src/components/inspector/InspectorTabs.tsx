import clsx from 'clsx'
import {useUIStore} from '@/stores/uiStore'

type Tab = 'details' | 'ai' | 'findings'

export default function InspectorTabs() {
    const tab = useUIStore(s => s.inspectorTab)
    const setTab = useUIStore(s => s.setInspectorTab)

    const tabs: {value: Tab; label: string}[] = [
        {value: 'details', label: 'Details'},
        {value: 'ai', label: 'AI'},
        {value: 'findings', label: 'Findings'},
    ]

    return (
        <div className="flex border-b border-border-default">
            {tabs.map(t => (
                <button
                    key={t.value}
                    className={clsx(
                        'flex-1 h-10 text-sm font-medium transition-colors duration-fast border-b-2',
                        tab === t.value
                            ? 'text-text-primary border-brand-500'
                            : 'text-text-secondary hover:text-text-primary border-transparent'
                    )}
                    onClick={() => setTab(t.value)}
                >
                    {t.label}
                </button>
            ))}
        </div>
    )
}
