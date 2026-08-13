import React, {useMemo, useCallback, useEffect, useRef, useState} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
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
import {EmptyState} from '@/components/shared'
import {useFlattenedBlocks, type FlatBlock} from '@/hooks/useFlattenedBlocks'
import {useKeyboard} from '@/hooks/useKeyboard'
import type {Severity, BlockType, Block} from '@/types'

// Block → occupants (remote users currently viewing it). Built once per
// presence change in BlockView and compared by reference in the item memo, so a
// presence update re-renders visible items the same way a findings-map change
// does.
type RemoteSelection = Map<string, PresenceUser[]>

function BlockItemWrapperComponent({
  item,
  findingCounts,
  findingSeverities,
  remoteSelection,
}: {
  item: FlatBlock
  findingCounts: Map<string, number>
  findingSeverities: Map<string, Severity>
  remoteSelection: RemoteSelection
}) {
  const {block, depth, isLast, collapsed} = item
  const selected = useFlowStore(s => s.selectedBlockId === block.id)
  const occupants = remoteSelection.get(block.id)

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
          onClick={() => useFlowStore.getState().selectBlock(block.id)}
        />
        {!isLast && !isContainerType(block.type) && <BlockConnector isActive={selected} />}
        {isContainerType(block.type) && collapsed && !isLast && <BlockConnector isActive={selected} />}
      </>
    )
  }, [block, selected, findingCounts, findingSeverities, collapsed, isLast, occupants])

  return (
    <div className="flex flex-col items-center w-full" style={{paddingLeft: depth * 20}}>
      {content}
    </div>
  )
}

function areEqual(
  prev: {
    item: FlatBlock
    findingCounts: Map<string, number>
    findingSeverities: Map<string, Severity>
    remoteSelection: RemoteSelection
  },
  next: {
    item: FlatBlock
    findingCounts: Map<string, number>
    findingSeverities: Map<string, Severity>
    remoteSelection: RemoteSelection
  },
) {
  if (!prev.item || !next.item) return false
  if (prev.findingCounts !== next.findingCounts) return false
  if (prev.findingSeverities !== next.findingSeverities) return false
  if (prev.remoteSelection !== next.remoteSelection) return false
  if (prev.item.block?.id !== next.item.block?.id) return false
  if (prev.item.depth !== next.item.depth) return false
  if (prev.item.isLast !== next.item.isLast) return false
  if (prev.item.collapsed !== next.item.collapsed) return false
  return true
}

const MemoizedBlockItemWrapper = React.memo(BlockItemWrapperComponent, areEqual)

export default function BlockView({subflowId}: {subflowId?: string} = {}) {
  const document = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const setVisibleBlockId = useFlowStore(s => s.setVisibleBlockId)
  const flattened = useFlattenedBlocks(subflowId)
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const [searchActive, setSearchActive] = useState(false)
  const [searchQuery, setSearchQuery] = useState('')
  const [searchMatchIdx, setSearchMatchIdx] = useState(0)
  // Subscribe to live presence so remote block selections render in the canvas.
  const presenceUsers = usePresenceStore(s => s.users)

  const displayedFlattened = useMemo(() => {
    if (!searchActive || !searchQuery.trim()) return flattened
    const q = searchQuery.toLowerCase()
    return flattened.filter(f => f.block.name.toLowerCase().includes(q))
  }, [flattened, searchActive, searchQuery])

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

  // Build block → occupants once per presence change. The map identity changes
  // whenever a collaborator's selection changes, which is what the item memo
  // compares — mirroring the findings-map pattern.
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
      useFlowStore.getState().selectBlock(flattened[next].block.id)
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
      useFlowStore.getState().selectBlock(findingBlockIds[next])
    },
    [flattened, selectedBlockId, findingCounts],
  )

  useKeyboard({
    scope: 'main',
    handlers: {
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
          useFlowStore.getState().selectBlock(cur.block.children[0].id)
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
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
        e.preventDefault()
        setSearchActive(true)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  useEffect(() => {
    setSearchActive(false)
    setSearchQuery('')
    setSearchMatchIdx(0)
  }, [subflowId])

  // Auto-scroll to first match when query changes
  useEffect(() => {
    if (searchActive && searchQuery.trim() && displayedFlattened.length > 0) {
      virtuosoRef.current?.scrollToIndex({index: 0, behavior: 'smooth', align: 'center'})
    }
  }, [searchQuery, searchActive])

  if (!document || flattened.length === 0) {
    return (
      <div className="w-full h-full flex items-center justify-center">
        <EmptyState
          title={document ? 'Empty subflow' : 'No flow loaded'}
          description={document ? 'This subflow has no blocks to display' : 'Open a flow file to begin'}
        />
      </div>
    )
  }

  return (
    <div className="block-view w-full h-full flex flex-col">
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
              />
            </div>
          )}
        />
      </div>
    </div>
  )
}
