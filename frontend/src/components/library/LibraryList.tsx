import {memo} from 'react'
import clsx from 'clsx'
import type {LibraryFlow} from '@/api/library'
import {relativeTime, absoluteTime} from '@/lib/time'

interface LibraryListProps {
  items: LibraryFlow[]
  selectedId: string | null
  onSelect: (flow: LibraryFlow) => void
  onOpen: (flow: LibraryFlow) => void
}

function LibraryListImpl({items, selectedId, onSelect, onOpen}: LibraryListProps) {
  return (
    <div className="rounded-lg border border-border-default bg-surface-2 overflow-hidden">
      <div className="grid grid-cols-[1fr_120px_140px_80px_80px] gap-3 px-4 py-2 border-b border-border-subtle bg-surface-3 text-2xs font-semibold uppercase tracking-wide text-text-tertiary">
        <div>Name</div>
        <div>Owner</div>
        <div>Updated</div>
        <div className="text-right">Blocks</div>
        <div className="text-right">Subflows</div>
      </div>
      <ul className="divide-y divide-border-subtle">
        {items.map(flow => {
          const selected = selectedId === flow.id
          return (
            <li
              key={flow.id}
              onClick={() => onSelect(flow)}
              onDoubleClick={() => onOpen(flow)}
              className={clsx(
                'grid grid-cols-[1fr_120px_140px_80px_80px] gap-3 px-4 py-2.5 text-sm cursor-pointer transition-colors',
                selected ? 'bg-brand-500/15 text-brand-300' : 'text-text-secondary hover:bg-surface-3',
              )}
            >
              <div className="min-w-0">
                <div className="font-medium text-text-primary truncate">{flow.name}</div>
                {flow.description && <div className="text-xs text-text-tertiary truncate">{flow.description}</div>}
              </div>
              <div className="text-xs truncate">{flow.ownerDisplayName ?? '—'}</div>
              <div className="text-xs" title={absoluteTime(flow.updatedAt)}>
                {relativeTime(flow.updatedAt)}
              </div>
              <div className="text-xs text-right tabular-nums">{flow.blockCount}</div>
              <div className="text-xs text-right tabular-nums">{flow.subflowCount}</div>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

export default memo(LibraryListImpl)
