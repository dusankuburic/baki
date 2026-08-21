import {useRef, useCallback, useMemo, useEffect, useState} from 'react'
import {Copy, ArrowDownToLine, Eye, EyeOff} from 'lucide-react'
import TreeNode from './TreeNode'
import {flattenTreeRows, type TreeRow} from '@/lib/tree'
import {writeClipboard} from '@/lib/clipboard'
import type {FlowDocument, BlockType, Highlight} from '@/types'

const ROW_HEIGHT = 28
const BUFFER = 200

type FlowTreeProps = {
  document: FlowDocument
  selectedBlockId: string | null
  visibleBlockId: string | null
  selectedSubflowId: string | null
  expandedSubflowIds: Set<string>
  expandedBlockIds: Set<string>
  visibleTypes: Set<BlockType>
  searchQuery?: string
  matchedBlockIds?: Set<string>
  searchHighlights?: Map<string, Highlight[]>
  onSelectBlock: (blockId: string, subflowId: string) => void
  onSelectSubflow: (subflowId: string) => void
  onToggleSubflowExpand: (id: string) => void
  onToggleBlockExpand: (id: string) => void
  findingCounts?: Map<string, number>
}

export default function FlowTree({
  document,
  selectedBlockId,
  visibleBlockId,
  selectedSubflowId,
  expandedSubflowIds,
  expandedBlockIds,
  visibleTypes,
  searchQuery,
  matchedBlockIds,
  searchHighlights,
  onSelectBlock,
  onSelectSubflow,
  onToggleSubflowExpand,
  onToggleBlockExpand,
  findingCounts,
}: FlowTreeProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportHeight, setViewportHeight] = useState(600)
  const [focusedIndex, setFocusedIndex] = useState(-1)
  const [ctxMenu, setCtxMenu] = useState<{row: TreeRow; x: number; y: number} | null>(null)
  const scrollRafRef = useRef<number | null>(null)

  const rows = useMemo(
    () => flattenTreeRows(document, {expandedSubflowIds, expandedBlockIds, visibleTypes, searchQuery, matchedBlockIds}),
    [document, expandedSubflowIds, expandedBlockIds, visibleTypes, searchQuery, matchedBlockIds],
  )
  // rows snapshot for the stable select handler (focus-index lookup without
  // re-creating the callback when rows change).
  const rowsRef = useRef(rows)
  rowsRef.current = rows

  useEffect(() => {
    const el = containerRef.current
    if (!el) return
    const observer = new ResizeObserver(entries => {
      for (const entry of entries) {
        setViewportHeight(entry.contentRect.height)
      }
    })
    observer.observe(el)
    return () => observer.disconnect()
  }, [])

  const totalHeight = rows.length * ROW_HEIGHT

  useEffect(() => {
    if (!visibleBlockId || !containerRef.current) return
    const index = rows.findIndex(r => r.id === visibleBlockId)
    if (index === -1) return

    const targetScrollTop = index * ROW_HEIGHT
    const currentScrollTop = containerRef.current.scrollTop

    // If block is outside or near the edges of the current viewport, scroll it in
    if (targetScrollTop < currentScrollTop || targetScrollTop > currentScrollTop + viewportHeight - ROW_HEIGHT * 2) {
      containerRef.current.scrollTo({
        top: Math.max(0, targetScrollTop - ROW_HEIGHT * 3), // Show 3 rows above for context
        behavior: 'auto', // Use auto for instant sync while scrolling
      })
    }
  }, [visibleBlockId, rows, viewportHeight])

  const startIndex = Math.max(0, Math.floor((scrollTop - BUFFER) / ROW_HEIGHT))
  const endIndex = Math.min(rows.length, Math.ceil((scrollTop + viewportHeight + BUFFER) / ROW_HEIGHT))
  const visibleRows = rows.slice(startIndex, endIndex)

  const handleScroll = useCallback(() => {
    // rAF-coalesced: scroll events fire at frame+ frequency, and each
    // setScrollTop re-render previously re-rendered every visible TreeNode
    // (before their callbacks were stabilized).
    if (scrollRafRef.current !== null) return
    scrollRafRef.current = requestAnimationFrame(() => {
      scrollRafRef.current = null
      if (containerRef.current) {
        setScrollTop(containerRef.current.scrollTop)
      }
    })
  }, [])

  useEffect(
    () => () => {
      if (scrollRafRef.current !== null) cancelAnimationFrame(scrollRafRef.current)
    },
    [],
  )

  // Stable row-carrying handlers (see TreeNode's props doc): identity never
  // changes, so TreeNode's memo holds across scroll/selection renders.
  const handleRowSelect = useCallback(
    (row: TreeRow) => {
      setFocusedIndex(-1)
      if (row.kind === 'subflow') {
        onSelectSubflow(row.id)
      } else {
        onSelectBlock(row.id, row.subflowId)
      }
      // Also move keyboard focus to the clicked row (the previous inline
      // closure set focusedIndex to the row's flat index).
      rowsRef.current.findIndex((r, i) => {
        if (r.id === row.id && r.kind === row.kind) {
          setFocusedIndex(i)
          return true
        }
        return false
      })
    },
    [onSelectSubflow, onSelectBlock],
  )

  const handleRowToggleExpand = useCallback(
    (row: TreeRow) => {
      if (row.kind === 'subflow') {
        onToggleSubflowExpand(row.id)
      } else {
        onToggleBlockExpand(row.id)
      }
    },
    [onToggleSubflowExpand, onToggleBlockExpand],
  )

  useEffect(() => {
    void setFocusedIndex(-1)
  }, [rows])

  useEffect(() => {
    if (!ctxMenu) return
    const handler = () => setCtxMenu(null)
    globalThis.document.addEventListener('click', handler)
    globalThis.document.addEventListener('contextmenu', handler)
    return () => {
      globalThis.document.removeEventListener('click', handler)
      globalThis.document.removeEventListener('contextmenu', handler)
    }
  }, [ctxMenu])

  const handleContextMenu = useCallback((row: TreeRow, x: number, y: number) => {
    setCtxMenu({row, x, y})
  }, [])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (rows.length === 0) return

      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault()
          setFocusedIndex(i => Math.min(i + 1, rows.length - 1))
          break
        case 'ArrowUp':
          e.preventDefault()
          setFocusedIndex(i => Math.max(i - 1, 0))
          break
        case 'ArrowRight': {
          e.preventDefault()
          const row = rows[focusedIndex]
          if (row && (row.hasChildren || row.kind === 'subflow')) {
            if (row.kind === 'subflow' && !expandedSubflowIds.has(row.id)) {
              onToggleSubflowExpand(row.id)
            } else if (row.kind === 'block' && !expandedBlockIds.has(row.id)) {
              onToggleBlockExpand(row.id)
            }
          }
          break
        }
        case 'ArrowLeft': {
          e.preventDefault()
          const row = rows[focusedIndex]
          if (row) {
            if (row.kind === 'subflow' && expandedSubflowIds.has(row.id)) {
              onToggleSubflowExpand(row.id)
            } else if (row.kind === 'block' && expandedBlockIds.has(row.id)) {
              onToggleBlockExpand(row.id)
            }
          }
          break
        }
        case 'Enter': {
          e.preventDefault()
          const row = rows[focusedIndex]
          if (!row) break
          if (row.kind === 'subflow') {
            onSelectSubflow(row.id)
          } else {
            onSelectBlock(row.id, row.subflowId)
          }
          break
        }
      }
    },
    [
      rows,
      focusedIndex,
      expandedSubflowIds,
      expandedBlockIds,
      onToggleSubflowExpand,
      onToggleBlockExpand,
      onSelectBlock,
      onSelectSubflow,
    ],
  )

  return (
    <div
      ref={containerRef}
      className="flex-1 overflow-y-auto overflow-x-hidden"
      onScroll={handleScroll}
      onKeyDown={handleKeyDown}
      tabIndex={0}
      role="tree"
    >
      <div style={{height: totalHeight, position: 'relative'}}>
        {visibleRows.map((row, i) => {
          const idx = startIndex + i
          const isSelected = row.kind === 'subflow' ? selectedSubflowId === row.id : selectedBlockId === row.id
          const isExpanded = row.kind === 'subflow' ? expandedSubflowIds.has(row.id) : expandedBlockIds.has(row.id)
          const isFocused = idx === focusedIndex
          const isViewportVisible = row.id === visibleBlockId

          return (
            <div
              key={`${row.kind}-${row.id}`}
              style={{
                position: 'absolute',
                top: idx * ROW_HEIGHT,
                left: 0,
                right: 0,
                height: ROW_HEIGHT,
              }}
            >
              <TreeNode
                row={row}
                isSelected={isSelected || isFocused}
                isExpanded={isExpanded}
                isViewportVisible={isViewportVisible}
                searchHighlight={searchQuery}
                highlights={row.kind === 'block' ? searchHighlights?.get(row.id) : undefined}
                isSearchMatch={matchedBlockIds?.has(row.id) ?? false}
                findingCount={findingCounts?.get(row.id) ?? 0}
                onSelect={handleRowSelect}
                onToggleExpand={handleRowToggleExpand}
                onContextMenu={handleContextMenu}
              />
            </div>
          )
        })}
      </div>
      {ctxMenu && (
        <div
          className="fixed z-overlay bg-surface-1 border border-border-default rounded-lg shadow-xl py-1 min-w-[160px] animate-fade-in"
          style={{left: ctxMenu.x, top: ctxMenu.y}}
        >
          <button
            className="flex items-center gap-2 w-full px-3 py-1.5 text-sm text-text-secondary hover:bg-surface-2 transition-colors"
            onClick={() => {
              void writeClipboard(ctxMenu.row.name)
              setCtxMenu(null)
            }}
          >
            <Copy size={14} />
            Copy name
          </button>
          {ctxMenu.row.kind === 'block' && ctxMenu.row.blockData && (
            <button
              className="flex items-center gap-2 w-full px-3 py-1.5 text-sm text-text-secondary hover:bg-surface-2 transition-colors"
              onClick={() => {
                const ln = ctxMenu.row.blockData?.lineNumber
                if (ln != null) void writeClipboard(ln.toString())
                setCtxMenu(null)
              }}
            >
              <Copy size={14} />
              Copy line number
            </button>
          )}
          {(ctxMenu.row.hasChildren || ctxMenu.row.kind === 'subflow') && (
            <button
              className="flex items-center gap-2 w-full px-3 py-1.5 text-sm text-text-secondary hover:bg-surface-2 transition-colors"
              onClick={() => {
                if (ctxMenu.row.kind === 'subflow') {
                  onToggleSubflowExpand(ctxMenu.row.id)
                } else {
                  onToggleBlockExpand(ctxMenu.row.id)
                }
                setCtxMenu(null)
              }}
            >
              {expandedSubflowIds.has(ctxMenu.row.id) || expandedBlockIds.has(ctxMenu.row.id) ? (
                <>
                  <EyeOff size={14} /> Collapse
                </>
              ) : (
                <>
                  <Eye size={14} /> Expand all
                </>
              )}
            </button>
          )}
          {ctxMenu.row.kind === 'subflow' && (
            <button
              className="flex items-center gap-2 w-full px-3 py-1.5 text-sm text-text-secondary hover:bg-surface-2 transition-colors"
              onClick={() => {
                onSelectSubflow(ctxMenu.row.id)
                setCtxMenu(null)
              }}
            >
              <ArrowDownToLine size={14} />
              Drill into subflow
            </button>
          )}
        </div>
      )}
    </div>
  )
}
