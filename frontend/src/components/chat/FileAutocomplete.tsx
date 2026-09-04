import {useTranslation} from 'react-i18next'
import {useEffect, useMemo} from 'react'
import clsx from 'clsx'
import type {SourceFileInfo} from '@/types'
import {FileText} from 'lucide-react'
import {useListNavigation} from '@/hooks/useListNavigation'

interface Props {
  query: string
  // The flow's source files, owned by AITab (useChatConversations already
  // fetches them once per document). Passing them down replaces a per-open
  // refetch of the same list.
  files: SourceFileInfo[]
  onSelect: (file: string) => void
  onClose: () => void
  // Reports how many entries the menu is showing so the composer knows whether
  // its keys belong to this menu — 0 means Enter must fall through and send.
  onMatchCount?: (n: number) => void
}

export default function FileAutocomplete({query, files, onSelect, onClose, onMatchCount}: Props) {
  const {t} = useTranslation('chat')

  const filtered = useMemo(() => {
    const q = query.toLowerCase()
    return files.filter(f => f.filename.toLowerCase().includes(q))
  }, [files, query])

  // Same keyboard contract as SlashCommandAutocomplete: arrows move, Enter/Tab
  // select, Escape closes. Before this the @-menu had no key handling at all
  // while ChatInput still swallowed every key it saw.
  const {
    activeIndex,
    setActiveIndex,
    handleKeyDown,
  } = useListNavigation({
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
    window.addEventListener('keydown', handleKeyDown as (e: KeyboardEvent) => void, true)
    return () => window.removeEventListener('keydown', handleKeyDown as (e: KeyboardEvent) => void, true)
  }, [handleKeyDown, filtered.length])

  if (filtered.length === 0) return null

  return (
    <div className="absolute bottom-full left-0 mb-2 w-[min(16rem,100%)] bg-surface-2 border border-border-default rounded-lg shadow-xl overflow-hidden animate-slide-up">
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
