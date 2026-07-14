import {useRef, useCallback, useEffect} from 'react'
import {Search, X} from 'lucide-react'
import Kbd from '@/components/shared/Kbd'
import {useSearchStore} from '@/stores/searchStore'

type SearchBarProps = {
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  resultCount?: number
  activeIndex?: number
  onNextResult?: () => void
  onPrevResult?: () => void
}

export default function SearchBar({
  value,
  onChange,
  disabled = false,
  resultCount,
  activeIndex,
  onNextResult,
  onPrevResult,
}: SearchBarProps) {
  const inputRef = useRef<HTMLInputElement>(null)
  const focusRequest = useSearchStore(s => s.focusRequest)

  useEffect(() => {
    if (focusRequest > 0) {
      requestAnimationFrame(() => inputRef.current?.focus())
    }
  }, [focusRequest])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') {
        onChange('')
        inputRef.current?.blur()
      } else if (e.key === 'Enter' && e.shiftKey) {
        onPrevResult?.()
      } else if (e.key === 'Enter') {
        onNextResult?.()
      }
    },
    [onChange, onNextResult, onPrevResult],
  )

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'f') {
        e.preventDefault()
        inputRef.current?.focus()
      }
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [])

  return (
    <div className="flex items-center h-12 px-3">
      <div className="flex items-center w-full h-8 bg-surface-2 border border-border-default rounded-md px-2 gap-2 focus-within:border-border-focus focus-within:ring-2 focus-within:ring-brand-500/20 transition-shadow duration-fast">
        <Search size={14} className="text-text-tertiary flex-shrink-0" />
        <input
          ref={inputRef}
          type="text"
          value={value}
          onChange={e => onChange(e.target.value)}
          onKeyDown={handleKeyDown}
          disabled={disabled}
          placeholder="Search blocks, variables, values…"
          className="flex-1 text-sm bg-transparent outline-none text-text-primary placeholder:text-text-tertiary disabled:opacity-50 disabled:cursor-not-allowed"
        />
        {value && (
          <>
            {resultCount !== undefined && (
              <span className="text-2xs text-text-tertiary tabular-nums">
                {activeIndex !== undefined ? `${activeIndex + 1}/` : ''}
                {resultCount}
              </span>
            )}
            <button onClick={() => onChange('')} className="text-text-tertiary hover:text-text-secondary">
              <X size={12} />
            </button>
          </>
        )}
        {!value && !disabled && <Kbd keys={['mod', 'f']} size="xs" className="opacity-60" />}
      </div>
    </div>
  )
}
