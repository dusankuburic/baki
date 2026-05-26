import clsx from 'clsx'
import type {BlockType} from '@/types/domain'
import {ALL_TYPES} from '@/stores/flowStore'

type FilterChip = {
    type: BlockType | 'ALL'
    label: string
    count: number
}

type FilterChipsProps = {
    chips: FilterChip[]
    activeTypes: Set<BlockType>
    onToggle: (type: BlockType) => void
    onSelectAll: () => void
}


export default function FilterChips({chips, activeTypes, onToggle, onSelectAll}: FilterChipsProps) {
    const allActive = activeTypes.size === ALL_TYPES.length || activeTypes.size === 0

    return (
        <div className="flex items-center h-9 px-3 gap-1 overflow-x-auto scrollbar-none">
            <button
                onClick={onSelectAll}
                className={clsx(
                    'flex-shrink-0 h-6 px-2 text-xs font-medium rounded-md transition-colors duration-fast',
                    allActive
                        ? 'bg-brand-500/15 text-brand-400 border border-brand-500/30'
                        : 'bg-surface-2 text-text-secondary border border-border-subtle hover:bg-surface-3'
                )}
            >
                All
            </button>
            {chips.filter(c => c.type !== 'ALL' && c.count > 0).map(chip => (
                <button
                    key={chip.type}
                    onClick={() => onToggle(chip.type as BlockType)}
                    className={clsx(
                        'flex-shrink-0 h-6 px-2 text-xs font-medium rounded-md transition-colors duration-fast flex items-center gap-1',
                        activeTypes.has(chip.type as BlockType)
                            ? 'bg-brand-500/15 text-brand-400 border border-brand-500/30'
                            : 'bg-surface-2 text-text-secondary border border-border-subtle hover:bg-surface-3'
                    )}
                >
                    {chip.label}
                    <span className="text-2xs opacity-60">{chip.count}</span>
                </button>
            ))}
        </div>
    )
}
