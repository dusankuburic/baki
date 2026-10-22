import React, {useMemo, useCallback, useEffect, useRef, useState} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {useTranslation} from 'react-i18next'
import BlockCard from './BlockCard'
import BlockConnector from './BlockConnector'
import BlockCaseSeparator from './BlockCaseSeparator'
import {writeClipboard} from '@/lib/clipboard'
import BlockElseSeparator from './BlockElseSeparator'
import BlockEnd, {isContainerType} from './BlockEnd'
import LoopControlBlock from './LoopControlBlock'
import {isLoopControl} from '@/lib/blocks'
import {useFlowStore} from '@/stores/flowStore'
import BlockSearchBar from './BlockSearchBar'
import {useAnalysisStore} from '@/stores/analysisStore'
import {usePresenceStore, type PresenceUser} from '@/stores/presenceStore'
import {EmptyState, useToast} from '@/components/shared'
import {useFlattenedBlocks, type FlatBlock} from '@/hooks/useFlattenedBlocks'
import {useIsDesktop} from '@/hooks/useMediaQuery'
import {flowApi} from '@/api'
import {runBlockEdit} from '@/lib/blockEdit'
import {useBlockOperations} from '@/lib/blockOperations'
import {useKeyboard} from '@/hooks/useKeyboard'
import {registerStoreReset} from '@/stores/storeRegistry'
import type {Severity, BlockType, Block} from '@/types'
import {getErrorMessage} from '@/lib/getErrorMessage'

type RemoteSelection = Map<string, PresenceUser[]>

const scrollerEls = new Map<string, HTMLElement>()
export const blockCanvasScrollers = scrollerEls
registerStoreReset(() => scrollerEls.clear())

const BLOCK_DND_MIME = 'application/x-baki-block'

const AUTOSCROLL_EDGE_PX = 64
const AUTOSCROLL_STEP_PX = 14

function BlockItemWrapperComponent({
  item,
  findingCounts,
  findingSeverities,
  remoteSelection,
  onShiftClick,
}: {
  item: FlatBlock
  findingCounts: Map<string, number>
  findingSeverities: Map<string, Severity>
  remoteSelection: RemoteSelection
  onShiftClick: (blockId: string) => void
}) {
  const {block, depth, isLast, collapsed} = item
  const {t} = useTranslation('flow')
  const selected = useFlowStore(s => s.selectedBlockId === block.id)
  const occupants = remoteSelection.get(block.id)

  const [dropPos, setDropPos] = useState<'before' | 'after' | null>(null)
  const toast = useToast()
  const readOnly = useFlowStore(s => s.readOnly)
  const isDesktop = useIsDesktop()
  const draggable = block.type !== 'END' && block.type !== 'ELSE' && block.type !== 'CASE' && block.type !== 'DEFAULT'
  const dragEnabled = draggable && !readOnly && isDesktop
  const handleDragStart = useCallback(
    (e: React.DragEvent) => {
      e.dataTransfer.setData(BLOCK_DND_MIME, block.id)
      e.dataTransfer.effectAllowed = 'move'
    },
    [block.id],
  )
  const handleDragOver = useCallback(
    (e: React.DragEvent) => {
      if (e.dataTransfer.types.includes(BLOCK_DND_MIME)) {
        e.preventDefault()
        e.dataTransfer.dropEffect = 'move'
        const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
        setDropPos(e.clientY < rect.top + rect.height / 2 ? 'before' : 'after')
        const sc = scrollerEls.get(block.subflowId || '__main__')
        if (sc) {
          const sr = sc.getBoundingClientRect()
          if (e.clientY < sr.top + AUTOSCROLL_EDGE_PX) sc.scrollTop -= AUTOSCROLL_STEP_PX
          else if (e.clientY > sr.bottom - AUTOSCROLL_EDGE_PX) sc.scrollTop += AUTOSCROLL_STEP_PX
        }
      }
    },
    [block.subflowId],
  )
  const handleDragLeave = useCallback((e: React.DragEvent) => {
    const to = e.relatedTarget as Node | null
    if (to && (e.currentTarget as HTMLElement).contains(to)) return
    setDropPos(null)
  }, [])
  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      if (!e.dataTransfer.types.includes(BLOCK_DND_MIME)) return
      e.preventDefault()
      setDropPos(null)
      const draggedId = e.dataTransfer.getData(BLOCK_DND_MIME)
      if (!draggedId || draggedId === block.id) return
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
      const position = e.clientY < rect.top + rect.height / 2 ? 'before' : 'after'
      const doc = useFlowStore.getState().document
      if (!doc) return
      runBlockEdit(doc.id, () => flowApi.moveBlockTo(doc.id, draggedId, block.id, position)).catch(err => {
        toast.error(t('canvas.moveFailed'), {description: getErrorMessage(err)})
      })
    },
    [block.id, toast, t],
  )

  const content = useMemo(() => {
    if (block.type === 'ELSE') {
      return <BlockElseSeparator blockId={block.id} collapsed={collapsed} />
    }

    if (block.type === 'CASE' || block.type === 'DEFAULT') {
      return <BlockCaseSeparator block={block} collapsed={collapsed} />
    }

    if (block.type === 'END') {
      return <BlockEnd label={block.name} parentType={block.properties._parentType as BlockType} />
    }

    if (isLoopControl(block.rawType)) {
      return (
        <LoopControlBlock
          block={block}
          selected={selected}
          onClick={() => useFlowStore.getState().selectBlock(block.id)}
        />
      )
    }

    return (
      <>
        <BlockCard
          block={block}
          selected={selected}
          hasFindings={(findingCounts.get(block.id) ?? 0) > 0}
          findingCount={findingCounts.get(block.id) ?? 0}
          findingSeverity={findingSeverities.get(block.id) ?? 'info'}
          remoteOccupants={occupants}
          onClick={() => {
            const st = useFlowStore.getState()
            if (st.selectedBlockIds.size > 0) st.clearBlockSelection()
            st.selectBlock(block.id)
          }}
          onShiftClick={onShiftClick}
        />
        {!isLast && !isContainerType(block.type) && <BlockConnector isActive={selected} />}
        {isContainerType(block.type) && collapsed && !isLast && <BlockConnector isActive={selected} />}
      </>
    )
  }, [block, selected, findingCounts, findingSeverities, collapsed, isLast, occupants, onShiftClick])

  const rails = useMemo(
    () =>
      Array.from({length: depth}, (_, i) => (
        <div
          key={i}
          className="absolute top-0 bottom-0 w-px bg-border-subtle pointer-events-none"
          style={{left: i * 20 + 9}}
          data-testid="indent-rail"
          data-depth={i + 1}
          aria-hidden="true"
        />
      )),
    [depth],
  )

  return (
    <div
      className="relative flex flex-col items-center w-full"
      style={{paddingLeft: depth * 20}}
      draggable={dragEnabled}
      onDragStart={dragEnabled ? handleDragStart : undefined}
      onDragOver={dragEnabled ? handleDragOver : undefined}
      onDragLeave={dragEnabled ? handleDragLeave : undefined}
      onDrop={dragEnabled ? handleDrop : undefined}
      data-block-id={block.id}
    >
      {rails}
      {dropPos === 'before' && (
        <div
          className="absolute left-0 right-0 top-0 h-0.5 bg-brand-400 z-10"
          data-testid="drop-indicator"
          aria-hidden="true"
        />
      )}
      <div
        data-testid="row-content"
        className="w-full flex flex-col items-center"
        style={{
          marginTop: dropPos === 'before' ? 10 : 0,
          marginBottom: dropPos === 'after' ? 10 : 0,
          transition: 'margin 150ms ease',
        }}
      >
        {content}
      </div>
      {dropPos === 'after' && (
        <div
          className="absolute left-0 right-0 bottom-0 h-0.5 bg-brand-400 z-10"
          data-testid="drop-indicator"
          aria-hidden="true"
        />
      )}
    </div>
  )
}

function areEqual(
  prev: {
    item: FlatBlock
    findingCounts: Map<string, number>
    findingSeverities: Map<string, Severity>
    remoteSelection: RemoteSelection
    onShiftClick: (blockId: string) => void
  },
  next: {
    item: FlatBlock
    findingCounts: Map<string, number>
    findingSeverities: Map<string, Severity>
    remoteSelection: RemoteSelection
    onShiftClick: (blockId: string) => void
  },
) {
  if (!prev.item || !next.item) return false
  if (prev.findingCounts !== next.findingCounts) return false
  if (prev.findingSeverities !== next.findingSeverities) return false
  if (prev.remoteSelection !== next.remoteSelection) return false
  if (prev.onShiftClick !== next.onShiftClick) return false
  if (prev.item.block?.id !== next.item.block?.id) return false
  if (prev.item.depth !== next.item.depth) return false
  if (prev.item.isLast !== next.item.isLast) return false
  if (prev.item.collapsed !== next.item.collapsed) return false
  return true
}

const MemoizedBlockItemWrapper = React.memo(BlockItemWrapperComponent, areEqual)

export default function BlockView({subflowId}: {subflowId?: string} = {}) {
  const {t} = useTranslation('flow')
  const document = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const setVisibleBlockId = useFlowStore(s => s.setVisibleBlockId)
  const flattened = useFlattenedBlocks(subflowId)
  const blockOps = useBlockOperations()
  const toast = useToast()
  const readOnly = useFlowStore(st => st.readOnly)
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const [searchActive, setSearchActive] = useState(false)
  const [searchQuery, setSearchQuery] = useState('')
  const [searchMatchIdx, setSearchMatchIdx] = useState(0)
  const presenceUsers = usePresenceStore(s => s.users)

  useEffect(() => {
    const key = subflowId ?? '__main__'
    return () => {
      scrollerEls.delete(key)
    }
  }, [subflowId])

  const displayedFlattened = useMemo(() => {
    if (!searchActive || !searchQuery.trim()) return flattened
    const q = searchQuery.toLowerCase()
    return flattened.filter(f => f.block.name.toLowerCase().includes(q))
  }, [flattened, searchActive, searchQuery])

  const handleShiftClick = useCallback(
    (blockId: string) => {
      const st = useFlowStore.getState()
      const anchor = st.selectedBlockId
      if (!anchor) {
        st.toggleBlockSelection(blockId)
        return
      }
      const ids = (searchActive ? displayedFlattened : flattened).map(f => f.block.id)
      const a = ids.indexOf(anchor)
      const b = ids.indexOf(blockId)
      if (a === -1 || b === -1) {
        st.toggleBlockSelection(blockId)
        return
      }
      const [from, to] = a < b ? [a, b] : [b, a]
      st.setBlockSelection(new Set(ids.slice(from, to + 1)))
    },
    [flattened, displayedFlattened, searchActive],
  )

  const groupMoveArgs = useCallback(
    (direction: 'up' | 'down'): {pivot: string; ref: string; position: 'before' | 'after'} | null => {
      const st = useFlowStore.getState()
      const doc = st.document
      const selected = [...st.selectedBlockIds]
      if (!doc || selected.length < 2) return null
      const editable = (b: Block) => !['END', 'ELSE', 'CASE', 'DEFAULT'].includes(b.type)
      const findScope = (blocks: Block[]): Block[] | null => {
        for (let i = 0; i < blocks.length; i++) {
          const b = blocks[i]
          if (!b) continue
          if (b.id === selected[0]) return blocks.filter(editable)
          const nested = findScope(b.children)
          if (nested) return nested
        }
        return null
      }
      let scope: Block[] | null = null
      for (const sf of doc.subflows) {
        scope = findScope(sf.blocks)
        if (scope) break
      }
      if (!scope) return null
      const idxs = selected.map(id => scope.findIndex(b => b.id === id))
      if (idxs.some(i => i === -1)) return null
      const sorted = [...idxs].sort((x, y) => x - y)
      for (let i = 1; i < sorted.length; i++) {
        if (sorted[i] !== (sorted[i - 1] ?? NaN) + 1) return null
      }
      const first = sorted[0]
      const last = sorted[sorted.length - 1]
      if (first === undefined || last === undefined) return null
      if (direction === 'up') {
        if (first === 0) return null
        const pivot = scope[first - 1]
        const ref = scope[last]
        if (!pivot || !ref) return null
        return {pivot: pivot.id, ref: ref.id, position: 'after'}
      }
      if (last === scope.length - 1) return null
      const pivotBelow = scope[last + 1]
      const refBelow = scope[first]
      if (!pivotBelow || !refBelow) return null
      return {pivot: pivotBelow.id, ref: refBelow.id, position: 'before'}
    },
    [],
  )

  const handleGroupMove = useCallback(
    (direction: 'up' | 'down') => {
      const st = useFlowStore.getState()
      const doc = st.document
      if (!doc) return
      const args = groupMoveArgs(direction)
      if (!args) {
        toast.info(t('canvas.groupMoveTitle'), {
          description: t('canvas.groupMoveBody'),
        })
        return
      }
      runBlockEdit(doc.id, () => flowApi.moveBlockTo(doc.id, args.pivot, args.ref, args.position)).catch(err =>
        toast.error(t('canvas.groupMoveFailed'), {description: getErrorMessage(err)}),
      )
    },
    [groupMoveArgs, toast, t],
  )

  const selectedCount = useFlowStore(st => st.selectedBlockIds.size)
  const clearBlockSelection = useFlowStore(st => st.clearBlockSelection)

  const handleRangeChanged = React.useCallback(
    (range: {startIndex: number; endIndex: number}) => {
      if (displayedFlattened && displayedFlattened.length > 0 && range.startIndex < displayedFlattened.length) {
        const first = displayedFlattened[range.startIndex]
        if (first?.block?.id) {
          setVisibleBlockId(first.block.id)
        }
      }
    },
    [displayedFlattened, setVisibleBlockId],
  )

  const report = useAnalysisStore(s => (document ? s.reports.get(document.id) : undefined))

  const {findingCounts, findingSeverities} = useMemo(() => {
    const counts = new Map<string, number>()
    const sevs = new Map<string, Severity>()
    if (report) {
      for (const f of report.findings) {
        counts.set(f.blockId, (counts.get(f.blockId) ?? 0) + 1)
        const existing = sevs.get(f.blockId)
        if (!existing || f.severity === 'error' || (f.severity === 'warning' && existing === 'info')) {
          sevs.set(f.blockId, f.severity)
        }
      }
    }
    return {findingCounts: counts, findingSeverities: sevs}
  }, [report])

  const remoteSelection = useMemo<RemoteSelection>(() => {
    const map: RemoteSelection = new Map()
    for (const u of Object.values(presenceUsers)) {
      if (!u.selectedBlockId) continue
      const arr = map.get(u.selectedBlockId)
      if (arr) arr.push(u)
      else map.set(u.selectedBlockId, [u])
    }
    return map
  }, [presenceUsers])

  useEffect(() => {
    if (!selectedBlockId || !flattened.length) return
    const index = flattened.findIndex(f => f.block.id === selectedBlockId)
    if (index === -1) return
    virtuosoRef.current?.scrollToIndex({index, behavior: 'smooth', align: 'center'})
  }, [selectedBlockId, flattened])

  const navigateRelative = useCallback(
    (delta: 1 | -1) => {
      if (!flattened.length) return
      const curIdx = selectedBlockId ? flattened.findIndex(f => f.block.id === selectedBlockId) : -1
      const next =
        curIdx === -1
          ? delta > 0
            ? 0
            : flattened.length - 1
          : Math.max(0, Math.min(flattened.length - 1, curIdx + delta))
      const target = flattened[next]
      if (target) useFlowStore.getState().selectBlock(target.block.id)
    },
    [flattened, selectedBlockId],
  )

  const goToMatch = useCallback(
    (delta: 1 | -1) => {
      if (!displayedFlattened.length) return
      const next = (searchMatchIdx + delta + displayedFlattened.length) % displayedFlattened.length
      setSearchMatchIdx(next)
      virtuosoRef.current?.scrollToIndex({index: next, behavior: 'smooth', align: 'center'})
    },
    [searchMatchIdx, displayedFlattened],
  )

  const navigateFinding = useCallback(
    (delta: 1 | -1) => {
      if (!flattened.length || !report) return
      const blockIds = flattened.map(f => f.block.id)
      const findingBlockIds = blockIds.filter(id => (findingCounts.get(id) ?? 0) > 0)
      if (findingBlockIds.length === 0) return
      const curIdx = selectedBlockId ? findingBlockIds.indexOf(selectedBlockId) : -1
      const next =
        curIdx === -1
          ? delta > 0
            ? 0
            : findingBlockIds.length - 1
          : (curIdx + delta + findingBlockIds.length) % findingBlockIds.length
      useFlowStore.getState().selectBlock(findingBlockIds[next] ?? null)
    },
    [flattened, selectedBlockId, findingCounts, report],
  )

  useKeyboard({
    scope: 'main',
    handlers: {
      'edit.move.up': () => {
        if (readOnly || !selectedBlockId) return
        void blockOps.moveBlock(selectedBlockId, 'up')
      },
      'edit.move.down': () => {
        if (readOnly || !selectedBlockId) return
        void blockOps.moveBlock(selectedBlockId, 'down')
      },
      'edit.duplicate': () => {
        if (readOnly || !selectedBlockId) return
        void blockOps.duplicateBlock(selectedBlockId)
      },
      'edit.delete': () => {
        const st = useFlowStore.getState()
        if (readOnly) return
        if (st.selectedBlockIds.size > 1) {
          void blockOps.removeBlocks([...st.selectedBlockIds])
          return
        }
        if (!selectedBlockId) return
        const block = st.selectedBlock()
        if (block) void blockOps.removeBlock(block.id, block.name, block.children.length)
      },
      'edit.rename': () => {
        const st = useFlowStore.getState()
        if (readOnly || !selectedBlockId) return
        const block = st.selectedBlock()
        if (!block) return
        if (block.rawType !== 'LABEL' && block.rawType !== 'COMMENT') {
          toast.info(t('canvas.derivedNameTitle'), {
            description: t('canvas.derivedNameBody'),
          })
          return
        }
        st.setRenamingBlock(selectedBlockId)
      },
      'nav.next.block': () => navigateRelative(1),
      'nav.prev.block': () => navigateRelative(-1),
      'nav.next.finding': () => navigateFinding(1),
      'nav.prev.finding': () => navigateFinding(-1),
      'nav.parent': () => {
        if (!selectedBlockId || !document) return
        const block = useFlowStore.getState().selectedBlock()
        if (block?.parentId) useFlowStore.getState().selectBlock(block.parentId)
      },
      'nav.first.child': () => {
        if (!selectedBlockId) return
        const cur = flattened.find(f => f.block.id === selectedBlockId)
        if (cur?.isContainer && !cur.collapsed && cur.block.children.length > 0) {
          useFlowStore.getState().selectBlock(cur.block.children[0]?.id ?? null)
        }
      },
      'nav.drill.subflow': () => {
        if (!selectedBlockId || !document) return
        const block = useFlowStore.getState().selectedBlock()
        if (!block) return
        if (block.rawType === 'CALL' || block.rawType === 'DISABLED_CALL') {
          const sfName = block.name
            .replace(/^Call\s+/i, '')
            .replace(' (disabled)', '')
            .trim()
          useFlowStore.getState().navigateToSubflowByName(sfName)
        }
      },
      'edit.copy.name': () => {
        const block = useFlowStore.getState().selectedBlock()
        if (block) writeClipboard(block.name).catch(() => {})
      },
      'edit.copy.path': () => {
        const {document: doc, selectedBlockId: bid} = useFlowStore.getState()
        if (!doc || !bid) return
        let path = ''
        for (const sf of doc.subflows) {
          const trail: string[] = []
          const found = (function search(blocks: Block[]): boolean {
            for (const b of blocks) {
              trail.push(b.name)
              if (b.id === bid) return true
              if (b.children?.length && search(b.children)) return true
              trail.pop()
            }
            return false
          })(sf.blocks)
          if (found) {
            path = `${sf.name} > ${trail.join(' > ')}`
            break
          }
        }
        if (path) writeClipboard(path).catch(() => {})
      },
      'edit.clear.selection': () => useFlowStore.getState().selectBlock(null),
    },
  })

  useEffect(() => {
    const onSearchOpen = () => setSearchActive(true)
    window.addEventListener('blockview:search-open', onSearchOpen)
    return () => window.removeEventListener('blockview:search-open', onSearchOpen)
  }, [])

  useEffect(() => {
    setSearchActive(false)
    setSearchQuery('')
    setSearchMatchIdx(0)
  }, [subflowId])

  useEffect(() => {
    if (searchActive && searchQuery.trim() && displayedFlattened.length > 0) {
      virtuosoRef.current?.scrollToIndex({index: 0, behavior: 'smooth', align: 'center'})
    }
  }, [searchQuery, searchActive, displayedFlattened.length])

  if (!document || flattened.length === 0) {
    return (
      <div className="w-full h-full flex items-center justify-center">
        <EmptyState
          title={document ? t('canvas.emptySubflowTitle') : t('canvas.noFlowTitle')}
          description={document ? t('canvas.emptySubflowBody') : t('canvas.noFlowBody')}
        />
      </div>
    )
  }

  return (
    <div className="block-view w-full h-full flex flex-col">
      {selectedCount > 1 && !readOnly && (
        <div
          className="flex items-center gap-2 px-3 py-1.5 border-b border-brand-500/30 bg-brand-500/5 text-2xs"
          data-testid="bulk-bar"
        >
          <span className="font-medium text-text-secondary">{t('canvas.selectedCount', {count: selectedCount})}</span>
          <button
            onClick={() => handleGroupMove('up')}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            {t('canvas.moveUp')}
          </button>
          <button
            onClick={() => handleGroupMove('down')}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            {t('canvas.moveDown')}
          </button>
          <button
            onClick={() => void blockOps.removeBlocks([...useFlowStore.getState().selectedBlockIds])}
            className="text-semantic-error px-1.5 py-0.5 rounded hover:bg-semantic-error/10 transition-colors"
          >
            {t('canvas.delete')}
          </button>
          <button
            onClick={clearBlockSelection}
            className="ml-auto text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            {t('canvas.clear')}
          </button>
        </div>
      )}
      {searchActive && (
        <BlockSearchBar
          query={searchQuery}
          onChange={q => {
            setSearchQuery(q)
            setSearchMatchIdx(0)
          }}
          matchIndex={searchMatchIdx}
          matchCount={displayedFlattened.length}
          onNext={() => goToMatch(1)}
          onPrev={() => goToMatch(-1)}
          onClose={() => {
            setSearchActive(false)
            setSearchQuery('')
            setSearchMatchIdx(0)
          }}
        />
      )}
      <div className="flex-1 min-h-0">
        <Virtuoso
          ref={virtuosoRef}
          scrollerRef={el => {
            const key = subflowId ?? '__main__'
            if (el instanceof HTMLElement) scrollerEls.set(key, el)
            else scrollerEls.delete(key)
          }}
          style={{height: '100%'}}
          data={displayedFlattened}
          computeItemKey={(_index, item) => item.block.id}
          rangeChanged={handleRangeChanged}
          itemContent={(_index, item) => (
            <div className="py-0.5">
              <MemoizedBlockItemWrapper
                item={item}
                findingCounts={findingCounts}
                findingSeverities={findingSeverities}
                remoteSelection={remoteSelection}
                onShiftClick={handleShiftClick}
              />
            </div>
          )}
        />
      </div>
    </div>
  )
}
