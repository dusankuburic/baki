import {useRef, useEffect} from 'react'
import {X, ChevronUp, ChevronDown, Search} from 'lucide-react'

interface Props {
  query: string
  onChange: (q: string) => void
  matchIndex: number
  matchCount: number
  onPrev: () => void
  onNext: () => void
  onClose: () => void
}

export default function BlockSearchBar({query, onChange, matchIndex, matchCount, onPrev, onNext, onClose}: Props) {
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  return (
    <div className="flex items-center gap-1.5 px-2 py-1.5 border-b border-border-subtle bg-surface-2 flex-shrink-0">
      <Search size={12} className="text-text-disabled shrink-0" />
      <input
        ref={inputRef}
        type="text"
        value={query}
        onChange={e => onChange(e.target.value)}
        placeholder="Search blocks..."
        className="flex-1 bg-transparent border-none outline-none text-xs text-text-primary placeholder:text-text-disabled min-w-0"
        onKeyDown={e => {
          if (e.key === 'Escape') { e.preventDefault(); onClose() }
          if (e.key === 'Enter') { e.preventDefault(); if (e.shiftKey) onPrev(); else onNext() }
        }}
      />
      {query && (
        <span className="text-2xs text-text-tertiary tabular-nums shrink-0 select-none">
          {matchCount === 0 ? 'No matches' : `${matchIndex + 1} / ${matchCount}`}
        </span>
      )}
      <button onClick={onPrev} disabled={matchCount === 0} className="p-0.5 text-text-tertiary hover:text-text-secondary disabled:opacity-40 transition-colors" title="Previous match (Shift+Enter)" aria-label="Previous match">
        <ChevronUp size={14} />
      </button>
      <button onClick={onNext} disabled={matchCount === 0} className="p-0.5 text-text-tertiary hover:text-text-secondary disabled:opacity-40 transition-colors" title="Next match (Enter)" aria-label="Next match">
        <ChevronDown size={14} />
      </button>
      <button onClick={onClose} className="p-0.5 text-text-tertiary hover:text-text-secondary transition-colors" title="Close search (Escape)" aria-label="Close search">
        <X size={14} />
      </button>
    </div>
  )
}
