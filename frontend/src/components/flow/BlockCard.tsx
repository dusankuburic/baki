import React, {useCallback, useEffect, useMemo, useState} from 'react'
import clsx from 'clsx'
import {
  ChevronDown,
  ChevronRight,
  Copy,
  CopyPlus,
  Search,
  Clock,
  Brain,
  Sparkles,
  FileText,
  ExternalLink,
  Trash2,
  ArrowUp,
  ArrowDown,
  PencilLine,
  type LucideIcon,
} from 'lucide-react'
import {getBlockIcon, getBlockColor, resolveTypeLabel, stripBlockKeywords} from '@/lib/blocks'
import {writeClipboard} from '@/lib/clipboard'
import TokenRenderer from './TokenRenderer'
import {isContainerType} from './BlockEnd'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'
import {useSearchStore} from '@/stores/searchStore'
import type {PresenceUser} from '@/stores/presenceStore'
import {stageBlockPrompt} from '@/lib/fixWithAI'
import type {Block} from '@/types'
import ContextMenu, {type ContextMenuItem} from '@/components/shared/ContextMenu'
import Avatar from '@/components/shared/Avatar'
import {Tooltip, useToast} from '@/components/shared'
import {useBlockOperations} from '@/lib/blockOperations'
import {analysisApi} from '@/api'
import BlockPropertiesModal from './BlockPropertiesModal'
import {logger} from '@/lib/logger'

type BlockCardProps = {
  block: Block
  selected?: boolean
  highlighted?: boolean
  hasFindings?: boolean
  findingCount?: number
  findingSeverity?: 'error' | 'warning' | 'info'
  remoteOccupants?: PresenceUser[]
  onClick?: () => void
  // Shift-click range selection (U3b) — BlockView owns the visible order.
  onShiftClick?: (blockId: string) => void
  onDoubleClick?: () => void
}

export default React.memo(function BlockCard({
  block,
  selected = false,
  highlighted = false,
  hasFindings = false,
  findingCount = 0,
  findingSeverity = 'error',
  remoteOccupants,
  onClick,
  onShiftClick,
  onDoubleClick,
}: BlockCardProps) {
  const [hovered, setHovered] = useState(false)
  const toast = useToast()
  const Icon = getBlockIcon(block.type) as LucideIcon
  const color = getBlockColor(block.type)
  const displayName = stripBlockKeywords(block.type, block.name)
  const stripeWidth = 4
  const isContainer = isContainerType(block.type)
  const collapsed = useFlowStore(s => isContainer && s.expandedBlockIds.has(block.id))
  const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
  const Chevron = collapsed ? ChevronRight : ChevronDown
  // Multi-select membership (V1.5): group-selected cards carry a visible
  // ring — before, only the bulk bar hinted that a selection existed.
  const inGroupSelection = useFlowStore(s => s.selectedBlockIds.has(block.id))
  const selectedVariable = useUIStore(s => s.selectedVariable)
  const complexityMode = useUIStore(s => s.complexityMode)
  const hasSelectedVar = selectedVariable && block.variables?.includes(selectedVariable)
  const isCallBlock = block.rawType === 'CALL' || block.rawType === 'DISABLED_CALL'
  const navigateToSubflowByName = useFlowStore(s => s.navigateToSubflowByName)
  const readOnly = useFlowStore(s => s.readOnly)

  const complexityScore = useMemo(() => {
    if (!complexityMode) return 0
    return block.indent + findingCount * 2
  }, [complexityMode, block.indent, findingCount])

  const complexityColor = useMemo(() => {
    if (!complexityMode) return undefined
    const score = Math.min(complexityScore, 10)
    if (score === 0) return undefined

    const opacity = 0.1 + score * 0.08
    const hue = 40 - score * 4
    return `hsla(${hue}, 100%, 50%, ${opacity})`
  }, [complexityMode, complexityScore])

  const [menuPos, setMenuPos] = useState<{x: number; y: number} | null>(null)

  const triggerAI = (text: string) => {
    const flowId = useFlowStore.getState().document?.id
    if (!flowId) return
    stageBlockPrompt(text, block.id, flowId)
  }

  const handleClick = useCallback(
    (e: React.MouseEvent) => {
      // Multi-select modifiers (U3b) outrank the CALL drill-through: plain
      // cmd/ctrl-click toggles selection; shift-click range-selects from the
      // current single-selection anchor (the range is computed by BlockView,
      // which owns the visible order).
      if (e.shiftKey) {
        e.stopPropagation()
        onShiftClick?.(block.id)
        return
      }
      if (e.metaKey || e.ctrlKey) {
        if (isCallBlock) {
          e.stopPropagation()
          const subflowName = block.name
            .replace(/^Call\s+/i, '')
            .replace(' (disabled)', '')
            .trim()
          navigateToSubflowByName(subflowName)
          return
        }
        e.stopPropagation()
        useFlowStore.getState().toggleBlockSelection(block.id)
        return
      }
      onClick?.()
    },
    [isCallBlock, block.id, block.name, navigateToSubflowByName, onClick, onShiftClick],
  )

  const handleContextMenu = (e: React.MouseEvent) => {
    e.preventDefault()
    setMenuPos({x: e.clientX, y: e.clientY})
  }

  // editBlock applies a visual-editor mutation (duplicate/remove) through the
  // server's block-edit endpoints and refreshes doc + findings — the edit is
  // parse-gated server-side and lands in the undo ring (snapshot endpoints).
  const [editingProps, setEditingProps] = useState(false)
  // Inline rename (U3b): only blocks whose name is user-controlled source
  // text (LABEL / COMMENT). Action names are derived — the backend refuses
  // with guidance, so the affordance never appears for them.
  const renaming = useFlowStore(st => st.renamingBlockId === block.id)
  const canRename = block.rawType === 'LABEL' || block.rawType === 'COMMENT'
  const [renameValue, setRenameValue] = useState(block.name)
  useEffect(() => {
    if (renaming) setRenameValue(block.name)
  }, [renaming, block.name])

  // Shared mutation implementation (U3b): the context menu, canvas keyboard
  // shortcuts, and the bulk bar all run these exact code paths.
  const {moveBlock, duplicateBlock, removeBlock, renameBlock} = useBlockOperations()
  const ops = {moveBlock, duplicateBlock, removeBlock}

  const contextItems: ContextMenuItem[] = [
    {
      label: 'Explain with AI',
      icon: Brain,
      onClick: () => triggerAI('Explain this block for me. What does it do in the context of the flow?'),
    },
  ]

  if (hasFindings) {
    contextItems.push({
      label: 'Fix with AI',
      icon: Sparkles,
      onClick: () => triggerAI('I have some findings for this block. Can you suggest how to fix them?'),
    })
  }

  if ((block.rawType === 'LABEL' || block.rawType === 'COMMENT') && !readOnly) {
    contextItems.push({
      label: 'Rename',
      icon: PencilLine,
      onClick: () => useFlowStore.getState().setRenamingBlock(block.id),
      shortcut: 'f2',
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
      },
    },
    {
      label: 'Copy Block ID',
      icon: Copy,
      onClick: () => {
        writeClipboard(block.id)
          .then(() => toast.success('Copied block ID'))
          .catch(() => toast.error('Copy failed'))
      },
    },
    // Read-only flows (view-only shares) hide direct mutations instead of
    // letting them fail with a server error after the fact.
    ...(readOnly
      ? []
      : [
          {
            label: 'Edit properties…',
            icon: PencilLine,
            onClick: () => setEditingProps(true),
          },
          {
            label: 'Move up',
            icon: ArrowUp,
            onClick: () => void ops.moveBlock(block.id, 'up'),
            shortcut: 'alt+up',
          },
          {
            label: 'Move down',
            icon: ArrowDown,
            onClick: () => void ops.moveBlock(block.id, 'down'),
            shortcut: 'alt+down',
          },
          {
            label: 'Duplicate block',
            icon: CopyPlus,
            onClick: () => void ops.duplicateBlock(block.id),
            shortcut: 'mod+d',
          },
          {
            label: 'Delete block',
            icon: Trash2,
            variant: 'danger' as const,
            onClick: () => void ops.removeBlock(block.id, block.name, block.children.length),
            shortcut: 'delete',
          },
        ]),
    {
      label: 'Find Usages',
      icon: Search,
      onClick: () => {
        const query =
          block.rawType === 'CALL' || block.rawType === 'DISABLED_CALL'
            ? block.name.replace('Call ', '').replace(' (disabled)', '')
            : block.name
        useSearchStore.getState().setQuery(query)
        if (useUIStore.getState().sidebarCollapsed) {
          useUIStore.getState().toggleSidebar()
        }
      },
    },
  )

  if (block.properties?._output) {
    contextItems.push({
      label: `Trace %${block.properties._output}%`,
      icon: Clock,
      onClick: async () => {
        try {
          const h = await analysisApi.getVariableLineage(block.properties._output)
          useAnalysisStore.getState().setVariableLineage(h)
          useUIStore.getState().setInspectorTab('details')
          if (useUIStore.getState().inspectorCollapsed) {
            useUIStore.getState().toggleInspector()
          }
        } catch (err) {
          logger.warn('Failed to trace:', err)
        }
      },
    })
  }

  return (
    <div
      data-block-id={block.id}
      role="button"
      tabIndex={0}
      aria-pressed={selected}
      aria-label={`${block.rawType}: ${block.name}`}
      onKeyDown={e => {
        if (e.key !== 'Enter' && e.key !== ' ') return
        e.preventDefault()
        e.stopPropagation()
        if (e.key === 'Enter' && selected && onDoubleClick) {
          onDoubleClick()
        } else {
          onClick?.()
        }
      }}
      className={clsx(
        'relative rounded-lg cursor-pointer transition-all duration-fast overflow-visible',
        'border max-w-[450px] w-full',
        'focus:outline-none focus-visible:ring-2 focus-visible:ring-brand-500/60',
        selected
          ? 'bg-surface-2 border-brand-500/50 shadow-glow scale-[1.01] animate-jump-pulse'
          : inGroupSelection
            ? 'bg-surface-2 border-brand-500/40 ring-1 ring-brand-500/30'
            : 'bg-surface-2 border-border-default shadow-sm',
        hovered && !selected && 'bg-surface-3 shadow-md',
        highlighted && 'ring-2 ring-semantic-warning/40',
        hasFindings && 'ring-1 ring-semantic-error/40',
        hasSelectedVar && 'ring-2 ring-yellow-400/50 bg-yellow-400/5',
      )}
      style={{
        padding: '12px',
        paddingLeft: `${12 + stripeWidth}px`,
        backgroundColor: complexityColor || undefined,
      }}
      onClick={handleClick}
      onDoubleClick={onDoubleClick}
      onContextMenu={handleContextMenu}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      {menuPos && <ContextMenu x={menuPos.x} y={menuPos.y} onClose={() => setMenuPos(null)} items={contextItems} />}
      {editingProps && <BlockPropertiesModal block={block} onClose={() => setEditingProps(false)} />}
      <div
        className="absolute left-0 top-0 bottom-0 overflow-hidden"
        style={{
          width: stripeWidth,
          backgroundColor: color,
          borderTopLeftRadius: '0.5rem',
          borderBottomLeftRadius: '0.5rem',
        }}
      />

      <div className="flex items-center gap-2">
        {isContainer ? (
          <button
            className="flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 transition-colors text-text-tertiary flex-shrink-0"
            onClick={e => {
              e.stopPropagation()
              toggleBlockExpand(block.id)
            }}
            aria-label={collapsed ? 'Expand block' : 'Collapse block'}
            aria-expanded={!collapsed}
          >
            <Chevron size={14} />
          </button>
        ) : (
          <span className="flex items-center justify-center w-4 h-4 flex-shrink-0">
            <Icon size={14} style={{color}} aria-hidden="true" />
          </span>
        )}
        <span className="flex items-center gap-1.5 min-w-0">
          {isContainer && <Icon size={12} style={{color}} className="opacity-70 flex-shrink-0" aria-hidden="true" />}
          <span className="text-sm font-medium uppercase tracking-wider truncate" style={{color}}>
            {resolveTypeLabel(block.type, block.name, block.rawType)}
          </span>
        </span>
        <span className="text-xs text-text-tertiary flex-shrink-0">L{block.lineNumber}</span>
        {collapsed && block.children.length > 0 && (
          <span className="text-xs text-text-tertiary flex-shrink-0">· {block.children.length} items</span>
        )}
        {!collapsed && block.properties && Object.keys(block.properties).length > 0 && (
          // V1.4: the count hints at config the card doesn't show — hovering
          // lists the user-facing properties (structural _ keys excluded)
          // instead of forcing a pane switch to the Details tab.
          <Tooltip
            side="bottom"
            content={
              <div className="max-h-48 overflow-y-auto">
                {Object.entries(block.properties)
                  .filter(([k]) => !k.startsWith('_'))
                  .slice(0, 12)
                  .map(([k, v]) => (
                    <div key={k} className="flex gap-2 whitespace-nowrap font-mono">
                      <span className="text-text-tertiary">{k}:</span>
                      <span className="text-text-primary truncate max-w-[220px]">{v}</span>
                    </div>
                  ))}
                {Object.keys(block.properties).filter(k => !k.startsWith('_')).length > 12 && (
                  <div className="text-text-tertiary">…more in Details</div>
                )}
              </div>
            }
          >
            <span className="text-xs text-text-tertiary flex-shrink-0 cursor-help" data-testid="props-hint">
              · {Object.keys(block.properties).length} props
            </span>
          </Tooltip>
        )}

        <span className="ml-auto flex items-center gap-2 min-w-0">
          {isCallBlock && hovered && (
            <span className="flex items-center gap-1 text-2xs text-text-disabled opacity-60 flex-shrink-0">
              <ExternalLink size={10} />
              <span>Ctrl+Click</span>
            </span>
          )}
          {remoteOccupants && remoteOccupants.length > 0 && (
            <span
              className="flex items-center -space-x-1.5 flex-shrink-0"
              title={remoteOccupants.map(u => u.displayName).join(', ')}
            >
              {remoteOccupants.slice(0, 3).map(u => (
                <Avatar
                  key={u.userId}
                  name={u.displayName}
                  colorSeed={u.userId}
                  avatarUrl={u.avatarUrl}
                  size="sm"
                  className="w-4 h-4 text-2xs ring-2 ring-surface-2"
                />
              ))}
              {remoteOccupants.length > 3 && (
                <span className="w-4 h-4 rounded-full bg-surface-3 ring-2 ring-surface-2 flex items-center justify-center text-2xs text-text-tertiary">
                  +{remoteOccupants.length - 3}
                </span>
              )}
            </span>
          )}
          {findingCount > 0 && (
            <span
              className="flex items-center justify-center min-w-4 h-4 px-1 text-2xs font-bold leading-none rounded-full border text-text-primary flex-shrink-0"
              title={`${findingCount} ${findingSeverity} finding${findingCount > 1 ? 's' : ''}`}
              style={{
                backgroundColor: `var(--${findingSeverity}-bg)`,
                borderColor: `var(--${findingSeverity})`,
              }}
            >
              {findingCount}
            </span>
          )}
        </span>
      </div>

      {renaming && !readOnly ? (
        <input
          autoFocus
          value={renameValue}
          onChange={e => setRenameValue(e.target.value)}
          onKeyDown={e => {
            e.stopPropagation()
            if (e.key === 'Enter') {
              const v = renameValue.trim()
              if (v && v !== block.name) void renameBlock(block.id, v)
              else useFlowStore.getState().setRenamingBlock(null)
            } else if (e.key === 'Escape') {
              useFlowStore.getState().setRenamingBlock(null)
            }
          }}
          onBlur={() => useFlowStore.getState().setRenamingBlock(null)}
          className="w-full text-[15px] font-medium text-text-primary bg-surface-1 border border-brand-500/50 rounded px-1.5 py-0.5 outline-none"
          aria-label="Rename block"
          data-testid="rename-input"
        />
      ) : (
        <div
          className="text-[15px] font-medium text-text-primary truncate"
          title={canRename ? `${displayName} (double-click to rename)` : displayName}
          onDoubleClick={e => {
            if (!canRename || readOnly) return
            e.stopPropagation()
            useFlowStore.getState().setRenamingBlock(block.id)
          }}
        >
          {block.tokens && block.tokens.length > 0 ? <TokenRenderer tokens={block.tokens} /> : displayName}
        </div>
      )}
    </div>
  )
})
