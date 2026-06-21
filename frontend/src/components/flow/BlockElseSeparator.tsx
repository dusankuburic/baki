import {ChevronDown, ChevronRight} from 'lucide-react'
import {getBlockColor} from '@/lib/blocks'
import {useFlowStore} from '@/stores/flowStore'

type BlockElseSeparatorProps = {
    blockId: string
    collapsed: boolean
}

export default function BlockElseSeparator({blockId, collapsed}: BlockElseSeparatorProps) {
    const color = getBlockColor('CONDITION')
    const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
    const Chevron = collapsed ? ChevronRight : ChevronDown

    return (
        <div
            className="relative flex items-center gap-2 max-w-[450px] w-full rounded-lg select-none overflow-hidden"
            style={{
                backgroundColor: 'var(--block-condition-bg)',
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
                    toggleBlockExpand(blockId)
                }}
            >
                <Chevron size={12} style={{color}} />
            </button>
            <span
                className="text-2xs font-semibold uppercase tracking-widest"
                style={{color}}
            >
                Else
            </span>
            {collapsed && (
                <span className="text-2xs text-text-tertiary ml-1">collapsed</span>
            )}
        </div>
    )
}
