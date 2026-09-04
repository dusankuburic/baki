import {useTranslation} from 'react-i18next'
import {useRef, useEffect} from 'react'
import {ChevronDown, ChevronUp, Search, X} from 'lucide-react'

interface Props {
  query: string
  onChange: (q: string) => void
  // Position within the match list, 1-based for display. 0 when there are none.
  current: number
  total: number
  onPrev: () => void
  onNext: () => void
  onClose: () => void
}

// Search steps THROUGH the conversation rather than filtering it: matches stay
// in place with their surrounding turns, occurrences are highlighted, and
// prev/next scrolls each one into view. Filtering hid the context that makes a
// hit meaningful, and gave no clue where in a long answer the match actually
// was.
export default function ChatSearchBar({query, onChange, current, total, onPrev, onNext, onClose}: Props) {
  const {t} = useTranslation('chat')
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const hasQuery = query.trim().length > 0

  return (
    <div className="flex items-center gap-1.5 px-3 py-1.5 border-b border-border-subtle bg-surface-2 flex-shrink-0">
      <Search size={12} className="text-text-disabled shrink-0" />
      <input
        ref={inputRef}
        type="text"
        value={query}
        onChange={e => onChange(e.target.value)}
        placeholder={t('search.placeholder')}
        aria-label={t('search.placeholder')}
        className="flex-1 bg-transparent border-none outline-none text-xs text-text-primary placeholder:text-text-disabled min-w-0"
        onKeyDown={e => {
          if (e.key === 'Escape') {
            e.preventDefault()
            onClose()
          } else if (e.key === 'Enter') {
            // Enter walks forward, Shift+Enter back — the find-bar convention.
            e.preventDefault()
            if (e.shiftKey) onPrev()
            else onNext()
          }
        }}
      />
      {hasQuery && (
        <span
          className="text-2xs text-text-tertiary tabular-nums shrink-0 select-none"
          role="status"
          aria-live="polite"
        >
          {total === 0 ? t('search.noMatches') : t('search.position', {current, total})}
        </span>
      )}
      <button
        type="button"
        onClick={onPrev}
        disabled={total === 0}
        className="p-0.5 rounded text-text-tertiary hover:text-text-secondary disabled:opacity-30 disabled:pointer-events-none transition-colors"
        title={t('search.previous')}
        aria-label={t('search.previous')}
      >
        <ChevronUp size={14} />
      </button>
      <button
        type="button"
        onClick={onNext}
        disabled={total === 0}
        className="p-0.5 rounded text-text-tertiary hover:text-text-secondary disabled:opacity-30 disabled:pointer-events-none transition-colors"
        title={t('search.next')}
        aria-label={t('search.next')}
      >
        <ChevronDown size={14} />
      </button>
      <button
        type="button"
        onClick={onClose}
        className="p-0.5 text-text-tertiary hover:text-text-secondary transition-colors"
        title={t('search.closeTitle')}
        aria-label={t('search.close')}
      >
        <X size={14} />
      </button>
    </div>
  )
}
