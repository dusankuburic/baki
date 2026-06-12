import React, {useCallback, useRef} from 'react'
import clsx from 'clsx'
import {
    ChevronRight, Box, Repeat, GitBranch,
    ShieldAlert, MessageSquare, Variable, Clock, BoxSelect, ArrowLeftRight, HelpCircle, Package,
    type LucideIcon,
} from 'lucide-react'
import type {Highlight} from '@/types/domain'
export type {TreeRow} from '@/lib/tree'
import type {TreeRow} from '@/lib/tree'

type TreeNodeProps = {
    row: TreeRow
    isSelected: boolean
    isExpanded: boolean
    isViewportVisible?: boolean
    onSelect: () => void
    onToggleExpand: () => void
    searchHighlight?: string
    highlights?: Highlight[]
    isSearchMatch?: boolean
    findingCount?: number
    onContextMenu?: (row: TreeRow, x: number, y: number) => void
}

const typeIcons: Record<string, LucideIcon> = {
    ACTION: Box,
    LOOP: Repeat,
    CONDITION: GitBranch,
    ERROR_HANDLER: ShieldAlert,
    COMMENT: MessageSquare,
    VARIABLE: Variable,
    WAIT: Clock,
    BLOCK: BoxSelect,
    SWITCH: ArrowLeftRight,
    UNKNOWN: HelpCircle,
}

const typeColors: Record<string, string> = {
    ACTION: 'text-block-action',
    LOOP: 'text-block-loop',
    CONDITION: 'text-block-condition',
    ERROR_HANDLER: 'text-block-error',
    COMMENT: 'text-block-comment',
    VARIABLE: 'text-block-variable',
    WAIT: 'text-block-wait',
    BLOCK: 'text-block-action',
    SWITCH: 'text-block-condition',
    UNKNOWN: 'text-text-tertiary',
}

function highlightText(text: string, query: string | undefined, highlights?: Highlight[]): React.ReactNode {
    if (highlights && highlights.length > 0) {
        const parts: React.ReactNode[] = []
        let last = 0
        for (const h of highlights) {
            if (h.start > last) {
                parts.push(text.slice(last, h.start))
            }
            parts.push(
                <mark key={h.start} className="bg-brand-500/30 text-text-primary rounded px-0.5">
                    {text.slice(h.start, h.end)}
                </mark>
            )
            last = h.end
        }
        if (last < text.length) {
            parts.push(text.slice(last))
        }
        return <>{parts}</>
    }
    if (!query || !query.trim()) return text
    const lower = text.toLowerCase()
    const qLower = query.toLowerCase()
    const idx = lower.indexOf(qLower)
    if (idx === -1) return text
    return (
        <>
            {text.slice(0, idx)}
            <mark className="bg-brand-500/30 text-text-primary rounded px-0.5">
                {text.slice(idx, idx + query.length)}
            </mark>
            {text.slice(idx + query.length)}
        </>
    )
}

function TreeNode({
    row,
    isSelected,
    isExpanded,
    isViewportVisible = false,
    onSelect,
    onToggleExpand,
    searchHighlight,
    highlights,
    isSearchMatch = false,
    findingCount = 0,
    onContextMenu,
}: TreeNodeProps) {
    const chevronRef = useRef<HTMLButtonElement>(null)

    const handleClick = useCallback((e: React.MouseEvent) => {
        if (row.hasChildren || row.kind === 'subflow') {
            const target = e.target as HTMLElement
            if (chevronRef.current?.contains(target)) {
                onToggleExpand()
                return
            }
        }
        onSelect()
    }, [row, onSelect, onToggleExpand])

    const indent = row.depth * 12
    const isSubflow = row.kind === 'subflow'
    const Icon = isSubflow ? Package : (row.blockType ? typeIcons[row.blockType] || HelpCircle : HelpCircle)
    const colorClass = isSubflow ? 'text-brand-400' : (row.blockType ? typeColors[row.blockType] : 'text-text-tertiary')

    return (
        <div
            className={clsx(
                'relative flex items-center h-7 cursor-pointer select-none group transition-colors duration-fast',
                isSelected ? 'bg-brand-500/10' : isSearchMatch ? 'bg-brand-500/5' : 'hover:bg-surface-2',
            )}
            style={{paddingLeft: indent + 4}}
            onClick={handleClick}
            onContextMenu={(e) => {
                e.preventDefault()
                onContextMenu?.(row, e.clientX, e.clientY)
            }}
            role="treeitem"
            aria-expanded={row.hasChildren ? isExpanded : undefined}
            aria-selected={isSelected}
        >
            {isViewportVisible && (
                <div className="absolute left-0 top-0 bottom-0 w-1 bg-brand-500/50" />
            )}
            {(row.hasChildren || isSubflow) && (
                <button
                    ref={chevronRef}
                    className={clsx(
                        'w-4 h-4 flex items-center justify-center flex-shrink-0 text-text-tertiary transition-transform duration-fast',
                        isExpanded && 'rotate-90',
                    )}
                    tabIndex={-1}
                >
                    <ChevronRight size={12} />
                </button>
            )}
            {!(row.hasChildren || isSubflow) && <div className="w-4 flex-shrink-0" />}

            <Icon size={14} className={clsx('flex-shrink-0', colorClass)} />

            <span className={clsx(
                'flex-1 text-sm truncate ml-1.5',
                isSubflow ? 'font-medium text-text-primary' : 'text-text-secondary',
            )}>
                {highlightText(row.name, searchHighlight, highlights)}
            </span>

            {findingCount > 0 && (
                <span className="flex-shrink-0 text-2xs bg-semantic-error/15 text-semantic-error px-1 rounded mr-2 tabular-nums">
                    {findingCount}
                </span>
            )}

            {row.hasChildren && !isExpanded && !findingCount && (
                <span className="flex-shrink-0 text-2xs text-text-tertiary tabular-nums mr-2">
                    {row.childCount}
                </span>
            )}
        </div>
    )
}

export default React.memo(TreeNode)
