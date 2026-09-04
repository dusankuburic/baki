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
import {EmptyState, useToast} from '@/components/shared'
import {useFlattenedBlocks, type FlatBlock} from '@/hooks/useFlattenedBlocks'
import {useIsDesktop} from '@/hooks/useMediaQuery'
import {flowApi} from '@/api'
import {refreshAfterBlockEdit} from '@/lib/blockEdit'
import {useBlockOperations} from '@/lib/blockOperations'
import {useKeyboard} from '@/hooks/useKeyboard'
import type {Severity, BlockType, Block} from '@/types'

// Block → occupants (remote users currently viewing it). Built once per
// presence change in BlockView and compared by reference in the item memo, so a
// presence update re-renders visible items the same way a findings-map change
// does.
type RemoteSelection = Map<string, PresenceUser[]>

// The Virtuoso scroller element, published for the drag-over edge auto-scroll
// (U3a.4). Module-scoped because exactly one BlockView owns the canvas at a
// time and rows are memoized — a prop would defeat the memo.
// One scroller PER SUBFLOW (F1.8): split editor groups render two canvases,
// and a single module slot meant the last-mounted instance won every
// drag-autoscroll — scrolling the wrong pane. Keyed by subflowId (rows know
// theirs via block.subflowId).
const scrollerEls = new Map<string, HTMLElement>()
// Exported for tests (drag-edge autoscroll needs a fake scroller element).
export const blockCanvasScrollers = scrollerEls

// DnD v2 (U3a.2): rows identify themselves with a custom MIME type so
// FOREIGN text/plain drags (selected text from this app or another) never
// light up the drop targets or mint garbage moveBlockTo calls.
const BLOCK_DND_MIME = 'application/x-baki-block'

// Edge auto-scroll geometry: start scrolling within this band of the
// scroller's top/bottom, this many px per dragover event (dragover fires
// continuously while hovering, which paces the scroll).
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
  const selected = useFlowStore(s => s.selectedBlockId === block.id)
  const occupants = remoteSelection.get(block.id)

  // Drag-to-reorder (R3.3): rows are draggable (structural markers are not);
  // a drop maps to moveBlockTo before/after this row, computed from the
  // cursor's half of the row. Cross-scope drops are refused server-side with
  // a guidance error surfaced as a toast here.
  const [dropPos, setDropPos] = useState<'before' | 'after' | null>(null)
  const toast = useToast()
  // View-only shares keep rows inert — no drag cursor, no drop handling.
  const readOnly = useFlowStore(s => s.readOnly)
  // HTML5 DnD never fires on touch (U5b): mobile users get the keyboard/menu
  // move paths instead of a dead drag affordance.
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
        // Edge auto-scroll (U3a.4): dragging past the viewport edge used to be
        // a dead end — the target rows weren't rendered yet. dragover paces us.
        const sc = scrollerEls.get(block.subflowId)
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
    // Child transitions fire dragleave on the row wrapper (F1.8): clearing
    // there made the indicator + gap animation flicker while crossing cards.
    const to = e.relatedTarget as Node | null
    if (to && (e.currentTarget as HTMLElement).contains(to)) return
    setDropPos(null)
  }, [])
  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      // Guard the MIME too (dragOver's check doesn't cover a drop that lands
      // without a prior dragOver on this row, e.g. foreign OS drags).
      if (!e.dataTransfer.types.includes(BLOCK_DND_MIME)) return
      e.preventDefault()
      setDropPos(null)
      const draggedId = e.dataTransfer.getData(BLOCK_DND_MIME)
      if (!draggedId || draggedId === block.id) return
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
      const position = e.clientY < rect.top + rect.height / 2 ? 'before' : 'after'
      const doc = useFlowStore.getState().document
      if (!doc) return
      flowApi
        .moveBlockTo(doc.id, draggedId, block.id, position)
        .then(res => {
          if (res?.document) refreshAfterBlockEdit(res.document)
        })
        .catch(err => {
          // Guards (not-a-sibling, stale-file) carry their own guidance.
          toast.error('Move failed', {description: String(err)})
        })
    },
    [block.id, toast],
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

  // Indentation guide rails (U3a.1): depth was invisible padding-only —
  // nested flows read as a flat list. One 1px vertical rail per ancestor
  // level, centered in each 20px gutter column, token-colored so themes
  // carry them. Rails span the full row height (content + connector).
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
      {/* Gap-open drop affordance (U3a.3): the row's CONTENT slides away
          from the drop edge instead of only a 2px line flashing — the space
          the block will occupy opens up. */}
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
  // onShiftClick closes over the flattened order (F1.3): without this check
  // rows keep a stale range-select callback after add/delete/move.
  if (prev.onShiftClick !== next.onShiftClick) return false
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
  const blockOps = useBlockOperations()
  const toast = useToast()
  const readOnly = useFlowStore(st => st.readOnly)
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

  // Shift-click range selection (U3b): anchor = current single selection,
  // range computed in VISIBLE (flattened) order — what the user sees.
  const handleShiftClick = useCallback(
    (blockId: string) => {
      const st = useFlowStore.getState()
      const anchor = st.selectedBlockId
      if (!anchor) {
        st.toggleBlockSelection(blockId)
        return
      }
      // Range in VISIBLE order (F1.3): while filtering, that's the searched
      // list — selecting hidden blocks through an invisible gap surprised.
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

  // Group move (U3b): a contiguous same-scope selection moves as a unit by
  // relocating the BOUNDARY sibling around it — one moveBlockTo call, no new
  // backend semantics. Returns the move args or a refusal reason.
  const groupMoveArgs = useCallback(
    (direction: 'up' | 'down'): {pivot: string; ref: string; position: 'before' | 'after'} | null => {
      const st = useFlowStore.getState()
      const doc = st.document
      const selected = [...st.selectedBlockIds]
      if (!doc || selected.length < 2) return null
      const editable = (b: Block) => !['END', 'ELSE', 'CASE', 'DEFAULT'].includes(b.type)
      // Returning finder (TS can't narrow closure-assigned locals). Scoped
      // per-subflow (F1.8): flatMapping all subflows merged distinct scopes
      // into one sibling list, letting cross-subflow selections pass the
      // contiguity check and rely on the server's refusal.
      const findScope = (blocks: Block[]): Block[] | null => {
        for (let i = 0; i < blocks.length; i++) {
          if (blocks[i].id === selected[0]) return blocks.filter(editable)
          const nested = findScope(blocks[i].children)
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
        if (sorted[i] !== sorted[i - 1] + 1) return null // non-contiguous
      }
      const first = sorted[0]
      const last = sorted[sorted.length - 1]
      if (direction === 'up') {
        if (first === 0) return null
        return {pivot: scope[first - 1].id, ref: scope[last].id, position: 'after'}
      }
      if (last === scope.length - 1) return null
      return {pivot: scope[last + 1].id, ref: scope[first].id, position: 'before'}
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
        toast.info('Group move needs a contiguous same-scope selection', {
          description:
            'Blocks in different containers (or with gaps) can’t move as one — drag or move them individually.',
        })
        return
      }
      flowApi
        .moveBlockTo(doc.id, args.pivot, args.ref, args.position)
        .then(res => {
          if (res?.document) refreshAfterBlockEdit(res.document)
        })
        .catch(err => toast.error('Group move failed', {description: String(err)}))
    },
    [groupMoveArgs, toast],
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
    [flattened, selectedBlockId, findingCounts, report],
  )

  useKeyboard({
    scope: 'main',
    handlers: {
      // Edit shortcuts (U3b) — the context-menu operations on the selected
      // block. Read-only flows ignore them (same gating as the menu).
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
          toast.info('Derived name', {
            description: "This block's name comes from its action type and properties — use Edit properties instead.",
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
    // mod+F routes here via useAppShortcuts' contextual dispatch (F1.8) —
    // the raw window listener fired BESIDE the global shortcut, lacked the
    // input guard, and never suppressed the browser's find bar.
    const onSearchOpen = () => setSearchActive(true) // BlockSearchBar autofocuses
    window.addEventListener('blockview:search-open', onSearchOpen)
    return () => window.removeEventListener('blockview:search-open', onSearchOpen)
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
  }, [searchQuery, searchActive, displayedFlattened.length])

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
      {/* Multi-select bulk bar (U3b) — the same operations as the context
          menu, applied to the selection set. */}
      {selectedCount > 1 && !readOnly && (
        <div
          className="flex items-center gap-2 px-3 py-1.5 border-b border-brand-500/30 bg-brand-500/5 text-2xs"
          data-testid="bulk-bar"
        >
          <span className="font-medium text-text-secondary">{selectedCount} blocks selected</span>
          <button
            onClick={() => handleGroupMove('up')}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            Move up
          </button>
          <button
            onClick={() => handleGroupMove('down')}
            className="text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            Move down
          </button>
          <button
            onClick={() => void blockOps.removeBlocks([...useFlowStore.getState().selectedBlockIds])}
            className="text-semantic-error px-1.5 py-0.5 rounded hover:bg-semantic-error/10 transition-colors"
          >
            Delete
          </button>
          <button
            onClick={clearBlockSelection}
            className="ml-auto text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            Clear
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
            // Virtuoso hands us the scroller element (or Window when using
            // window scrolling — not this canvas's mode).
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
