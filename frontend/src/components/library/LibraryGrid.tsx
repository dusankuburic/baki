import {memo} from 'react'
import clsx from 'clsx'
import {FileCode, Users, Clock} from 'lucide-react'
import {Spinner} from '@/components/shared'
import type {LibraryFlow} from '@/api/library'
import {relativeTime, absoluteTime} from '@/lib/time'

interface LibraryGridProps {
  items: LibraryFlow[]
  selectedId: string | null
  onSelect: (flow: LibraryFlow) => void
  onOpen: (flow: LibraryFlow) => void
  // Per-card loading (U4.5): the flow currently being fetched for opening.
  openingId?: string | null
}

function LibraryGridImpl({items, selectedId, onSelect, onOpen, openingId}: LibraryGridProps) {
  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4 gap-3">
      {items.map(flow => (
        <SelectableCard
          key={flow.id}
          flow={flow}
          selected={selectedId === flow.id}
          opening={openingId === flow.id}
          onClick={() => onSelect(flow)}
          onDoubleClick={() => onOpen(flow)}
          onKeyboardOpen={() => onOpen(flow)}
        />
      ))}
    </div>
  )
}

function SelectableCard({
  flow,
  selected,
  opening,
  onClick,
  onDoubleClick,
  onKeyboardOpen,
}: {
  flow: LibraryFlow
  selected: boolean
  opening?: boolean
  onClick: () => void
  onDoubleClick: () => void
  onKeyboardOpen: () => void
}) {
  const updated = relativeTime(flow.updatedAt)
  return (
    <button
      type="button"
      onClick={onClick}
      onDoubleClick={onDoubleClick}
      onKeyDown={e => {
        // Keyboard parity with the list (U4.5): Enter opens when already
        // selected, selects otherwise — Enter-on-focused-card used to be a
        // dead end below xl where the detail panel's Open button is hidden.
        if (e.key === 'Enter' && selected) {
          e.preventDefault()
          onKeyboardOpen()
        }
      }}
      disabled={opening}
      className={clsx(
        'group flex flex-col gap-3 p-4 rounded-xl border bg-surface-2 transition-colors text-left w-full',
        selected
          ? 'border-brand-500 ring-2 ring-brand-500/30 bg-surface-3'
          : 'border-border-default hover:border-brand-500/50 hover:bg-surface-3',
      )}
      aria-pressed={selected}
    >
      <div className="flex items-start gap-3">
        <div className="flex-shrink-0 w-9 h-9 rounded-lg bg-brand-500/10 flex items-center justify-center">
          {opening ? <Spinner size={16} /> : <FileCode size={18} className="text-brand-500" />}
        </div>
        <div className="flex-1 min-w-0">
          <p className="text-sm font-semibold text-text-primary truncate group-hover:text-brand-400 transition-colors">
            {flow.name}
          </p>
          {flow.description && <p className="mt-0.5 text-xs text-text-tertiary line-clamp-2">{flow.description}</p>}
        </div>
      </div>

      <div className="flex items-center gap-3 text-xs text-text-tertiary">
        <span className="flex items-center gap-1">
          <FileCode size={12} />
          {flow.blockCount}
        </span>
        {flow.ownerDisplayName && (
          <span className="flex items-center gap-1 truncate min-w-0">
            <Users size={12} className="flex-shrink-0" />
            <span className="truncate">{flow.ownerDisplayName}</span>
          </span>
        )}
        <span className="flex items-center gap-1 ml-auto flex-shrink-0" title={absoluteTime(flow.updatedAt)}>
          <Clock size={12} />
          {updated}
        </span>
      </div>

      {flow.isSharedWithMe && (
        <span className="self-start text-2xs font-medium px-1.5 py-0.5 rounded bg-brand-500/10 text-brand-400">
          Shared
        </span>
      )}
    </button>
  )
}

export default memo(LibraryGridImpl)
