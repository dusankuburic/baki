import {memo} from 'react'
import clsx from 'clsx'
import type {LibraryFlow} from '@/api/library'
import {relativeTime, absoluteTime} from '@/lib/time'
import {Spinner} from '@/components/shared'

interface LibraryListProps {
  items: LibraryFlow[]
  selectedId: string | null
  onSelect: (flow: LibraryFlow) => void
  onOpen: (flow: LibraryFlow) => void
  openingId?: string | null
}

// The columns are ranked (see .library-row in index.css): the name always
// shows and takes what is left; owner, then subflows, drop out as the results
// pane narrows. The header and the rows share the same classes, so a column
// and its heading always disappear together — the old fixed pixel tracks
// crushed the name to nothing and clipped "Subflows" while both headings
// still showed.
function LibraryListImpl({items, selectedId, onSelect, onOpen, openingId}: LibraryListProps) {
  return (
    <div className="library-list rounded-lg border border-border-default bg-surface-2 overflow-hidden">
      <div className="library-row px-4 py-2 border-b border-border-subtle bg-surface-3 text-2xs font-semibold uppercase tracking-wide text-text-tertiary">
        <div className="truncate">Name</div>
        <div className="library-col-owner truncate">Owner</div>
        <div className="truncate">Updated</div>
        <div className="text-right">Blocks</div>
        <div className="library-col-subflows text-right">Subflows</div>
      </div>
      <ul className="divide-y divide-border-subtle">
        {items.map(flow => {
          const selected = selectedId === flow.id
          return (
            <li
              key={flow.id}
              role="button"
              tabIndex={0}
              aria-pressed={selected}
              onClick={() => onSelect(flow)}
              onDoubleClick={() => onOpen(flow)}
              onKeyDown={e => {
                // Keyboard parity: Enter/Space select; Enter opens when
                // already selected (double-click equivalent).
                if (e.key !== 'Enter' && e.key !== ' ') return
                e.preventDefault()
                if (e.key === 'Enter' && selected) {
                  onOpen(flow)
                } else {
                  onSelect(flow)
                }
              }}
              className={clsx(
                'library-row px-4 py-2.5 text-sm cursor-pointer transition-colors',
                'focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand-500/60',
                selected ? 'bg-brand-500/15 text-brand-300' : 'text-text-secondary hover:bg-surface-3',
              )}
            >
              <div className="flex items-center gap-2 min-w-0">
                {/* The row being opened shows its spinner, like the grid card. */}
                {openingId === flow.id && <Spinner size={12} className="shrink-0" />}
                <div className="min-w-0">
                  <div className="font-medium text-text-primary truncate">{flow.name}</div>
                  {flow.description && <div className="text-xs text-text-tertiary truncate">{flow.description}</div>}
                </div>
              </div>
              <div className="library-col-owner text-xs truncate">{flow.ownerDisplayName ?? '—'}</div>
              <div className="text-xs truncate" title={absoluteTime(flow.updatedAt)}>
                {relativeTime(flow.updatedAt)}
              </div>
              <div className="text-xs text-right tabular-nums">{flow.blockCount}</div>
              <div className="library-col-subflows text-xs text-right tabular-nums">{flow.subflowCount}</div>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

export default memo(LibraryListImpl)
