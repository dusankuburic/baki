import {useTranslation} from 'react-i18next'
import {useEffect, useMemo} from 'react'
import clsx from 'clsx'
import type {SourceFileInfo} from '@/types'
import {FileText} from 'lucide-react'
import {useListNavigation} from '@/hooks/useListNavigation'
import {isImeKeydown} from '@/lib/imeKey'

interface Props {
  query: string
  files: SourceFileInfo[]
  onSelect: (file: string) => void
  onClose: () => void
  onMatchCount?: (n: number) => void
  isOwnTarget?: (target: EventTarget | null) => boolean
}

export default function FileAutocomplete({query, files, onSelect, onClose, onMatchCount, isOwnTarget}: Props) {
  const {t} = useTranslation('chat')

  const filtered = useMemo(() => {
    const q = query.toLowerCase()
    return files.filter(f => f.filename.toLowerCase().includes(q))
  }, [files, query])

  const {activeIndex, setActiveIndex, handleKeyDown} = useListNavigation({
    count: filtered.length,
    onSelect: i => {
      if (filtered[i]) onSelect(filtered[i].filename)
    },
    onClose,
    mode: 'wrap',
    extraSelectKeys: ['Tab'],
  })

  useEffect(() => {
    setActiveIndex(0)
  }, [query, setActiveIndex])

  useEffect(() => {
    onMatchCount?.(filtered.length)
  }, [filtered.length, onMatchCount])

  useEffect(() => {
    if (filtered.length === 0) return
    const onKey = (e: KeyboardEvent) => {
      if (isOwnTarget && !isOwnTarget(e.target)) return
      if (isImeKeydown(e)) return
      handleKeyDown(e)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [handleKeyDown, filtered.length, isOwnTarget])

  if (filtered.length === 0) return null

  return (
    <div
      className="absolute bottom-full left-0 mb-2 w-[min(16rem,100%)] bg-surface-2 border border-border-default rounded-lg shadow-xl overflow-hidden animate-slide-up"
      onMouseDown={e => e.preventDefault()}
    >
      <div className="px-3 py-2 bg-surface-3 border-b border-border-subtle">
        <span className="text-2xs font-semibold text-text-tertiary uppercase tracking-wider">{t('files.heading')}</span>
      </div>
      <div className="max-h-48 overflow-y-auto p-1 custom-scrollbar">
        {filtered.map((file, i) => (
          <button
            key={file.filename}
            className={clsx(
              'w-full flex items-center gap-2 px-3 py-2 text-sm rounded-md transition-colors text-left',
              i === activeIndex ? 'bg-brand-500/10 text-brand-400' : 'text-text-secondary hover:bg-surface-3',
            )}
            onClick={() => onSelect(file.filename)}
          >
            <FileText size={14} className="shrink-0" />
            <span className="truncate">{file.filename}</span>
          </button>
        ))}
      </div>
    </div>
  )
}
