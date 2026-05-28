import { FileCode, Users, Clock } from 'lucide-react'
import clsx from 'clsx'
import type { LibraryFlow } from '@/api/library'

interface FlowCardProps {
  flow: LibraryFlow
  onOpen?: (flow: LibraryFlow) => void
  className?: string
}

export default function FlowCard({ flow, onOpen, className }: FlowCardProps) {
  const updatedDate = new Date(flow.updatedAt).toLocaleDateString(undefined, {
    month: 'short', day: 'numeric', year: 'numeric',
  })

  return (
    <button
      onClick={() => onOpen?.(flow)}
      className={clsx(
        'group flex flex-col gap-3 p-4 rounded-xl border border-border-default bg-surface-2',
        'hover:border-brand-500/50 hover:bg-surface-3 transition-colors text-left w-full',
        className,
      )}
    >
      <div className="flex items-start gap-3">
        <div className="flex-shrink-0 w-9 h-9 rounded-lg bg-brand-500/10 flex items-center justify-center">
          <FileCode size={18} className="text-brand-500" />
        </div>
        <div className="flex-1 min-w-0">
          <p className="text-sm font-semibold text-text-primary truncate group-hover:text-brand-400 transition-colors">
            {flow.name}
          </p>
          {flow.description && (
            <p className="mt-0.5 text-xs text-text-muted line-clamp-2">{flow.description}</p>
          )}
        </div>
      </div>

      <div className="flex items-center gap-3 text-xs text-text-muted">
        <span className="flex items-center gap-1">
          <FileCode size={12} />
          {flow.blockCount} blocks
        </span>
        {flow.ownerDisplayName && (
          <span className="flex items-center gap-1">
            <Users size={12} />
            {flow.ownerDisplayName}
          </span>
        )}
        <span className="flex items-center gap-1 ml-auto">
          <Clock size={12} />
          {updatedDate}
        </span>
      </div>

      {flow.isSharedWithMe && (
        <span className="self-start text-[10px] font-medium px-1.5 py-0.5 rounded bg-brand-500/10 text-brand-400">
          Shared
        </span>
      )}
    </button>
  )
}
