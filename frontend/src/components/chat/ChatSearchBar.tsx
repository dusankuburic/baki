import {useRef, useEffect} from 'react'
import {X, Search} from 'lucide-react'

interface Props {
  query: string
  onChange: (q: string) => void
  matchCount: number
  onClose: () => void
}

export default function ChatSearchBar({query, onChange, matchCount, onClose}: Props) {
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  return (
    <div className="flex items-center gap-1.5 px-3 py-1.5 border-b border-border-subtle bg-surface-2 flex-shrink-0">
      <Search size={12} className="text-text-disabled shrink-0" />
      <input
        ref={inputRef}
        type="text"
        value={query}
        onChange={e => onChange(e.target.value)}
        placeholder="Search messages..."
        className="flex-1 bg-transparent border-none outline-none text-xs text-text-primary placeholder:text-text-disabled min-w-0"
        onKeyDown={e => {
          if (e.key === 'Escape') { e.preventDefault(); onClose() }
        }}
      />
      {query && (
        <span className="text-2xs text-text-tertiary tabular-nums shrink-0 select-none">
          {matchCount === 0 ? 'No matches' : `${matchCount} message${matchCount !== 1 ? 's' : ''}`}
        </span>
      )}
      <button
        onClick={onClose}
        className="p-0.5 text-text-tertiary hover:text-text-secondary transition-colors"
        title="Close search (Escape)"
        aria-label="Close search"
      >
        <X size={14} />
      </button>
    </div>
  )
}
