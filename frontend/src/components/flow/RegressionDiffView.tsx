import {useMemo, useRef, useState} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {useUIStore} from '@/stores/uiStore'
import {EmptyState} from '@/components/shared'
import {History, Plus, Minus, FileText, ArrowUp, ArrowDown} from 'lucide-react'
import clsx from 'clsx'
import type {BlockDiff, SubflowDiff} from '@/types/domain'
import {getBlockIcon} from '@/lib/blocks'
import IconButton from '@/components/shared/IconButton'

export default function RegressionDiffView() {
  const diff = useUIStore(s => s.activeDiff)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const [currentIndex, setCurrentIndex] = useState(-1)
  
  // Flatten the diff for virtualization — must be before any early return to satisfy rules-of-hooks
  const items = useMemo(() => {
    if (!diff) return []
    const list: Array<{type: 'header'; data: SubflowDiff} | {type: 'block'; data: BlockDiff}> = []
    for (const sf of diff.subflows) {
      list.push({type: 'header', data: sf})
      for (const b of sf.blocks) {
        list.push({type: 'block', data: b})
      }
    }
    return list
  }, [diff])

  const changeIndices = useMemo(() => {
    return items
      .map((item, idx) => item.type === 'block' && item.data.change !== 'none' ? idx : -1)
      .filter(idx => idx !== -1)
  }, [items])

  if (!diff) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0">
        <EmptyState
          icon={History}
          title="No comparison active"
          description="Select a file to compare with the current flow"
        />
      </div>
    )
  }

  const jumpToChange = (direction: 'next' | 'prev') => {
    if (changeIndices.length === 0) return

    let nextIdx: number
    if (direction === 'next') {
      const found = changeIndices.find(idx => idx > currentIndex)
      nextIdx = found !== undefined ? found : changeIndices[0]
    } else {
      const reversed = [...changeIndices].reverse()
      const found = reversed.find(idx => idx < currentIndex)
      nextIdx = found !== undefined ? found : changeIndices[changeIndices.length - 1]
    }

    setCurrentIndex(nextIdx)
    virtuosoRef.current?.scrollToIndex({
      index: nextIdx,
      align: 'center',
      behavior: 'smooth'
    })
  }

  return (
    <div className="flex-1 flex flex-col bg-surface-0 overflow-hidden">
      <div className="h-10 px-4 bg-surface-1 border-b border-border-default flex items-center justify-between shrink-0">
        <div className="flex items-center gap-4">
          <div className="flex items-center gap-2 text-xs font-bold text-text-secondary">
            <History size={14} />
            <span>STRUCTURAL DIFF</span>
          </div>

          {changeIndices.length > 0 && (
            <div className="flex items-center gap-1 border-l border-border-subtle pl-4 ml-2">
              <span className="text-[10px] text-text-tertiary mr-2 font-medium">
                {currentIndex !== -1 ? changeIndices.indexOf(currentIndex) + 1 : 0} / {changeIndices.length} CHANGES
              </span>
              <IconButton 
                icon={ArrowUp} 
                size="sm" 
                label="Previous Change" 
                onClick={() => jumpToChange('prev')} 
              />
              <IconButton 
                icon={ArrowDown} 
                size="sm" 
                label="Next Change" 
                onClick={() => jumpToChange('next')} 
              />
            </div>
          )}
        </div>
        <button 
          onClick={() => setMainPaneView('block')}
          className="text-[10px] font-black uppercase tracking-widest text-brand-500 hover:text-brand-400 transition-colors"
        >
          Exit Diff Mode
        </button>
      </div>

      <Virtuoso
        ref={virtuosoRef}
        data={items}
        itemContent={(_index, item) => {
          if (item.type === 'header') {
            return (
              <div className="px-6 py-4 bg-surface-1 border-b border-border-default sticky top-0 z-10 flex items-center gap-3">
                <FileText size={16} className="text-text-tertiary" />
                <h3 className="text-sm font-bold text-text-primary">Subflow: {item.data.name}</h3>
                {item.data.change !== 'none' && (
                  <span className={clsx(
                    "text-[10px] font-black uppercase px-1.5 py-0.5 rounded",
                    item.data.change === 'added' && "bg-semantic-success/10 text-semantic-success",
                    item.data.change === 'removed' && "bg-semantic-error/10 text-semantic-error",
                    item.data.change === 'modified' && "bg-semantic-warning/10 text-semantic-warning"
                  )}>
                    {item.data.change}
                  </span>
                )}
              </div>
            )
          }

          const block = item.data.new || item.data.old
          if (!block) return null
          
          const Icon = getBlockIcon(block.type)
          const isAdded = item.data.change === 'added'
          const isRemoved = item.data.change === 'removed'

          return (
            <div className={clsx(
              "px-6 py-1 flex group transition-colors",
              isAdded && "bg-semantic-success/5 hover:bg-semantic-success/10",
              isRemoved && "bg-semantic-error/5 hover:bg-semantic-error/10"
            )}>
              {/* Diff Gutter */}
              <div className="w-8 flex flex-col items-center shrink-0 border-r border-border-subtle/30 mr-4">
                {isAdded && <Plus size={12} className="text-semantic-success mt-1" />}
                {isRemoved && <Minus size={12} className="text-semantic-error mt-1" />}
                <span className="text-[10px] font-mono text-text-tertiary mt-auto">{block.lineNumber}</span>
              </div>

              {/* Indent Spacer */}
              <div style={{ width: block.indent * 20 }} className="shrink-0" />

              {/* Block Content */}
              <div className="flex-1 flex items-center gap-3 py-1.5 min-w-0">
                <Icon size={16} className={clsx(
                   isAdded ? "text-semantic-success" : isRemoved ? "text-semantic-error" : "text-text-tertiary"
                )} />
                <span className={clsx(
                  "text-sm font-medium truncate",
                  isAdded && "text-semantic-success",
                  isRemoved && "text-semantic-error line-through",
                  !isAdded && !isRemoved && "text-text-primary"
                )}>
                  {block.name}
                </span>
                
                {item.data.change === 'modified' && (
                  <span className="text-[10px] text-semantic-warning font-bold bg-semantic-warning/10 px-1.5 rounded ml-auto">
                    MODIFIED
                  </span>
                )}
              </div>
            </div>
          )
        }}
      />
    </div>
  )
}
