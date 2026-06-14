import {ChevronDown, ChevronRight} from 'lucide-react'
import {getBlockColor} from '@/lib/blocks'
import {useFlowStore} from '@/stores/flowStore'
import type {Block} from '@/types'

type BlockCaseSeparatorProps = {
    block: Block
    collapsed: boolean
}

export default function BlockCaseSeparator({block, collapsed}: BlockCaseSeparatorProps) {
    const color = getBlockColor('SWITCH')
    const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
    const Chevron = collapsed ? ChevronRight : ChevronDown

    const isDefault = block.type === 'DEFAULT'
    const rawLabel = isDefault ? '' : block.name.replace(/^CASE\s+/i, '')
    const label = rawLabel.replace(/^\s*=\s*/, '').replace(/^\$'''/, '').replace(/'''$/, '')

    return (
        <div
            className="relative flex items-center gap-2 max-w-[450px] w-full rounded-lg select-none overflow-hidden"
            style={{
                backgroundColor: 'rgba(16, 185, 129, 0.06)',
                padding: '8px 10px 8px 14px',
            }}
        >
            <div
                className="absolute left-0 top-0 bottom-0 rounded-l-lg"
                style={{
                    width: 4,
                    backgroundColor: color,
                }}
            />
            <button
                className="flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 transition-colors flex-shrink-0"
                onClick={(e) => {
                    e.stopPropagation()
                    toggleBlockExpand(block.id)
                }}
            >
                <Chevron size={12} style={{color}} />
            </button>
            <span
                className="text-2xs font-semibold uppercase tracking-widest flex-shrink-0"
                style={{color}}
            >
                {isDefault ? 'Default' : 'Case'}
            </span>
            {!isDefault && label && (
                <span className="text-2xs text-text-secondary truncate">
                    {label}
                </span>
            )}
            {collapsed && block.children.length > 0 && (
                <span className="text-2xs text-text-tertiary ml-auto flex-shrink-0">
                    {block.children.length} items
                </span>
            )}
        </div>
    )
}
