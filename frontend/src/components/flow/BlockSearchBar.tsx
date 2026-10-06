import {useRef, useEffect} from 'react'
import {useTranslation} from 'react-i18next'
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
  const {t} = useTranslation('flow')
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  return (
    <div className="flex items-center gap-1.5 px-2 py-1.5 border-b border-border-subtle bg-surface-2 shrink-0">
      <Search size={12} className="text-text-disabled shrink-0" />
      <input
        ref={inputRef}
        type="text"
        value={query}
        onChange={e => onChange(e.target.value)}
        placeholder={t('search.placeholder')}
        className="flex-1 bg-transparent border-none outline-hidden text-xs text-text-primary placeholder:text-text-disabled min-w-0"
        onKeyDown={e => {
          if (e.key === 'Escape') {
            e.preventDefault()
            onClose()
          }
          if (e.key === 'Enter') {
            e.preventDefault()
            if (e.shiftKey) onPrev()
            else onNext()
          }
        }}
      />
      {query && (
        <span className="text-2xs text-text-tertiary tabular-nums shrink-0 select-none">
          {matchCount === 0 ? t('search.noMatches') : t('search.position', {current: matchIndex + 1, total: matchCount})}
        </span>
      )}
      <button
        onClick={onPrev}
        disabled={matchCount === 0}
        className="p-0.5 text-text-tertiary hover:text-text-secondary disabled:opacity-40 transition-colors"
        title={t('search.prevTitle')}
        aria-label={t('search.prevAria')}
      >
        <ChevronUp size={14} />
      </button>
      <button
        onClick={onNext}
        disabled={matchCount === 0}
        className="p-0.5 text-text-tertiary hover:text-text-secondary disabled:opacity-40 transition-colors"
        title={t('search.nextTitle')}
        aria-label={t('search.nextAria')}
      >
        <ChevronDown size={14} />
      </button>
      <button
        onClick={onClose}
        className="p-0.5 text-text-tertiary hover:text-text-secondary transition-colors"
        title={t('search.closeTitle')}
        aria-label={t('search.closeAria')}
      >
        <X size={14} />
      </button>
    </div>
  )
}
