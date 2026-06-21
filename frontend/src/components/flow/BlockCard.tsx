import React, {useState, useMemo, useCallback} from 'react'
import clsx from 'clsx'
import {AlertTriangle, ChevronDown, ChevronRight, Copy, Search, Clock, Brain, Sparkles, FileText, ExternalLink, type LucideIcon} from 'lucide-react'
import {getBlockIcon, getBlockColor, resolveTypeLabel, stripBlockKeywords} from '@/lib/blocks'
import {writeClipboard} from '@/lib/clipboard'
import TokenRenderer from './TokenRenderer'
import {isContainerType} from './BlockEnd'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import {useSearchStore} from '@/stores/searchStore'
import type {Block, VariableHistory} from '@/types'
import ContextMenu, {type ContextMenuItem} from '@/components/shared/ContextMenu'
import {useToast} from '@/components/shared'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'

type BlockCardProps = {
    block: Block
    selected?: boolean
    highlighted?: boolean
    hasFindings?: boolean
    findingCount?: number
    findingSeverity?: 'error' | 'warning' | 'info'
    onClick?: () => void
    onDoubleClick?: () => void
}


export default React.memo(function BlockCard({
    block,
    selected = false,
    highlighted = false,
    hasFindings = false,
    findingCount = 0,
    findingSeverity = 'error',
    onClick,
    onDoubleClick,
}: BlockCardProps) {
    const [hovered, setHovered] = useState(false)
    const toast = useToast()
    const Icon = getBlockIcon(block.type) as LucideIcon
    const color = getBlockColor(block.type)
    const displayName = stripBlockKeywords(block.type, block.name)
    const stripeWidth = (selected || hovered) ? 6 : 4
    const isContainer = isContainerType(block.type)
    const collapsed = useFlowStore(s => isContainer && s.expandedBlockIds.has(block.id))
    const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
    const Chevron = collapsed ? ChevronRight : ChevronDown
    const selectedVariable = useUIStore(s => s.selectedVariable)
    const complexityMode = useUIStore(s => s.complexityMode)
    const hasSelectedVar = selectedVariable && block.variables?.includes(selectedVariable)
    const isCallBlock = block.rawType === 'CALL' || block.rawType === 'DISABLED_CALL'
    const navigateToSubflowByName = useFlowStore(s => s.navigateToSubflowByName)

    const complexityScore = useMemo(() => {
        if (!complexityMode) return 0
        return block.indent + (findingCount * 2)
    }, [complexityMode, block.indent, findingCount])

    const complexityColor = useMemo(() => {
        if (!complexityMode) return undefined
        const score = Math.min(complexityScore, 10)
        if (score === 0) return undefined
        
        // Heatmap scale: Yellow -> Orange -> Red
        const hue = 40 - (score * 4) // 40 (Yellow) to 0 (Red)
        const opacity = 0.1 + (score * 0.08)
        return `hsla(${hue}, 100%, 50%, ${opacity})`
    }, [complexityMode, complexityScore])

    const [menuPos, setMenuPos] = useState<{x: number; y: number} | null>(null)

    const setPendingMessage = useChatStore(s => s.setPendingMessage)
    const setInspectorTab = useUIStore(s => s.setInspectorTab)
    const setInspectorCollapsed = useUIStore(s => s.setInspectorCollapsed)

    const triggerAI = (text: string) => {
        setPendingMessage({text, contextBlockId: block.id})
        setInspectorTab('ai')
        setInspectorCollapsed(false)
    }

    const handleClick = useCallback((e: React.MouseEvent) => {
        if ((e.ctrlKey || e.metaKey) && isCallBlock) {
            e.stopPropagation()
            const subflowName = block.name.replace(/^Call\s+/i, '').replace(' (disabled)', '').trim()
            navigateToSubflowByName(subflowName)
            return
        }
        onClick?.()
    }, [isCallBlock, block.name, navigateToSubflowByName, onClick])

    const handleContextMenu = (e: React.MouseEvent) => {
        e.preventDefault()
        setMenuPos({x: e.clientX, y: e.clientY})
    }

    const contextItems: ContextMenuItem[] = [
        {
            label: 'Explain with AI',
            icon: Brain,
            onClick: () => triggerAI('Explain this block for me. What does it do in the context of the flow?')
        }
    ]

    if (hasFindings) {
        contextItems.push({
            label: 'Fix with AI',
            icon: Sparkles,
            onClick: () => triggerAI('I have some findings for this block. Can you suggest how to fix them?')
        })
    }

    contextItems.push(
        {
            label: 'Copy as Markdown',
            icon: FileText,
            onClick: () => {
                let md = `### ${block.name} (${block.rawType})\n\n`
                if (block.properties && Object.keys(block.properties).length > 0) {
                    md += '**Properties:**\n'
                    for (const [k, v] of Object.entries(block.properties)) {
                        if (k.startsWith('_')) continue
                        md += `- **${k}**: ${v}\n`
                    }
                }
                if (block.variables?.length) {
                    md += `\n**Variables:** ${block.variables.join(', ')}\n`
                }
                writeClipboard(md)
                    .then(() => toast.success('Copied as Markdown'))
                    .catch(() => toast.error('Copy failed'))
            }
        },
        {
            label: 'Copy Block ID',
            icon: Copy,
            onClick: () => {
                writeClipboard(block.id)
                    .then(() => toast.success('Copied block ID'))
                    .catch(() => toast.error('Copy failed'))
            }
        },
        {
            label: 'Find Usages',
            icon: Search,
            onClick: () => {
                const query = block.rawType === 'CALL' || block.rawType === 'DISABLED_CALL' 
                    ? block.name.replace('Call ', '').replace(' (disabled)', '')
                    : block.name
                useSearchStore.getState().setQuery(query)
                if (useUIStore.getState().sidebarCollapsed) {
                    useUIStore.getState().toggleSidebar()
                }
            }
        }
    )

    if (block.properties?._output) {
        contextItems.push({
            label: `Trace %${block.properties._output}%`,
            icon: Clock,
            onClick: async () => {
                try {
                    const h = await analysisApi.getVariableLineage(block.properties._output)
                    useAnalysisStore.getState().setVariableLineage(h as unknown as VariableHistory)
                    useUIStore.getState().setInspectorTab('details')
                    if (useUIStore.getState().inspectorCollapsed) {
                        useUIStore.getState().toggleInspector()
                    }
                } catch (err) {
                    logger.warn('Failed to trace:', err)
                }
            }
        })
    }

    return (
        <div
            data-block-id={block.id}
            className={clsx(
                'relative rounded-lg cursor-pointer transition-all duration-fast overflow-visible',
                'border max-w-[450px] w-full',
                selected
                    ? 'bg-surface-2 border-brand-500/50 shadow-glow scale-[1.01] animate-jump-pulse'
                    : 'bg-surface-2 border-border-default shadow-sm',
                hovered && !selected && 'bg-surface-3 shadow-md',
                highlighted && 'ring-2 ring-semantic-warning/40',
                hasFindings && 'ring-1 ring-semantic-error/40',
                hasSelectedVar && 'ring-2 ring-yellow-400/50 bg-yellow-400/5',
            )}
            style={{
                padding: '12px', 
                paddingLeft: `${12 + stripeWidth}px`,
                backgroundColor: complexityColor || undefined
            }}
            onClick={handleClick}
            onDoubleClick={onDoubleClick}
            onContextMenu={handleContextMenu}
            onMouseEnter={() => setHovered(true)}
            onMouseLeave={() => setHovered(false)}
        >
            {menuPos && (
                <ContextMenu
                    x={menuPos.x}
                    y={menuPos.y}
                    onClose={() => setMenuPos(null)}
                    items={contextItems}
                />
            )}
            <div
                className="absolute left-0 top-0 bottom-0 overflow-hidden"
                style={{
                    width: stripeWidth,
                    backgroundColor: color,
                    borderTopLeftRadius: '0.5rem',
                    borderBottomLeftRadius: '0.5rem',
                }}
            />

            {findingCount > 0 && (
                <div
                    className={clsx(
                        'absolute -top-1.5 -right-1.5 flex items-center gap-0.5 text-2xs font-bold px-1.5 py-0.5 rounded-full shadow-sm',
                        findingSeverity === 'error' && 'bg-semantic-error text-white',
                        findingSeverity === 'warning' && 'bg-semantic-warning text-white',
                        findingSeverity === 'info' && 'bg-semantic-info text-white',
                    )}
                >
                    <AlertTriangle size={10} />
                    {findingCount}
                </div>
            )}

            <div className="flex items-center gap-2">
                <span className="flex items-center justify-center w-4 h-4 flex-shrink-0">
                    {isContainer && (
                        <button
                            className="flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 transition-colors"
                            onClick={(e) => {
                                e.stopPropagation()
                                toggleBlockExpand(block.id)
                            }}
                        >
                            <Chevron size={14} style={{color}} />
                        </button>
                    )}
                </span>
                <Icon size={14} style={{color}} />
                <span className="text-sm font-medium uppercase tracking-wider" style={{color}}>
                    {resolveTypeLabel(block.type, block.name, block.rawType)}
                </span>
                <span className="text-xs text-text-tertiary">
                    L{block.lineNumber}
                </span>
                {collapsed && block.children.length > 0 && (
                    <span className="text-xs text-text-tertiary">
                        · {block.children.length} items
                    </span>
                )}
                {!collapsed && block.properties && Object.keys(block.properties).length > 0 && (
                    <span className="text-xs text-text-tertiary">
                        · {Object.keys(block.properties).length} props
                    </span>
                )}
                {isCallBlock && hovered && (
                    <span className="ml-auto flex items-center gap-1 text-2xs text-text-disabled opacity-60">
                        <ExternalLink size={10} />
                        <span>Ctrl+Click</span>
                    </span>
                )}
            </div>

            <div className="text-[15px] font-medium text-text-primary truncate" title={displayName}>
                {block.tokens && block.tokens.length > 0 ? (
                    <TokenRenderer tokens={block.tokens} />
                ) : (
                    displayName
                )}
            </div>
        </div>
    )
})
