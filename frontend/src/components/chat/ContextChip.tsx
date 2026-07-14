import {X, Crosshair} from 'lucide-react'

interface Props {
  blockId: string
  blockName?: string
  blockType?: string
  onClear: () => void
}

export default function ContextChip({blockId, blockName, blockType, onClear}: Props) {
  if (!blockId) return null

  return (
    <div className="flex items-center gap-2 px-3 py-2 bg-brand-500/8 border border-brand-500/15 rounded-lg mx-3">
      <Crosshair size={12} className="text-brand-400 shrink-0" />
      <div className="flex-1 min-w-0">
        <span className="text-xs text-text-secondary font-medium truncate block">{blockName || blockId}</span>
        {blockType && <span className="text-2xs text-text-tertiary">{blockType}</span>}
      </div>
      <button
        className="p-0.5 rounded hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors shrink-0"
        onClick={onClear}
        aria-label="Clear context"
      >
        <X size={12} />
      </button>
    </div>
  )
}
