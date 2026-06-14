import clsx from 'clsx'
import {getBlockColor} from '@/lib/blocks'
import type {BlockType} from '@/types'

const containerTypes: Set<BlockType> = new Set([
    'LOOP', 'CONDITION', 'BLOCK', 'ERROR_HANDLER', 'SWITCH',
])

export function isContainerType(type: BlockType): boolean {
    return containerTypes.has(type)
}

type BlockEndProps = {
    label?: string
    parentType?: BlockType
}

export default function BlockEnd({label, parentType}: BlockEndProps) {
    const color = parentType ? getBlockColor(parentType) : '#64748b'

    return (
        <div
            className={clsx(
                'flex items-center gap-2 max-w-[450px] w-full',
                'px-3 py-1.5 rounded-lg border border-dashed',
                'bg-gray-50/50 dark:bg-gray-900/30 border-gray-200 dark:border-gray-800',
            )}
        >
            <div
                className="w-1.5 h-1.5 rounded-full flex-shrink-0"
                style={{backgroundColor: color}}
            />
            <span className="text-2xs font-semibold uppercase tracking-wider opacity-60" style={{color}}>
                {label || (parentType ? `End ${parentType === 'ERROR_HANDLER' ? 'Error' : parentType}` : 'End')}
            </span>
        </div>
    )
}
