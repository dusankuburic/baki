import {getBlockIcon, getBlockColor} from '@/lib/blocks'
import type {Block} from '@/types/domain'

type ChildrenListProps = {
    children: Block[]
    onSelect: (blockId: string) => void
}

export default function ChildrenList({children, onSelect}: ChildrenListProps) {
    if (children.length === 0) return null

    return (
        <div className="space-y-0.5">
            {children.map(child => {
                const Icon = getBlockIcon(child.type)
                const color = getBlockColor(child.type)
                return (
                    <button
                        key={child.id}
                        className="flex items-center gap-2 w-full h-8 px-2 text-sm text-text-secondary hover:bg-surface-2 rounded transition-colors duration-fast"
                        onClick={() => onSelect(child.id)}
                    >
                        <Icon size={14} style={{color}} />
                        <span className="truncate">{child.name}</span>
                    </button>
                )
            })}
        </div>
    )
}
