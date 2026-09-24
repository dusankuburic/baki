import {useTranslation} from 'react-i18next'
import {useEffect, useMemo} from 'react'
import clsx from 'clsx'
import type {SourceFileInfo} from '@/types'
import {FileText} from 'lucide-react'
import {useListNavigation} from '@/hooks/useListNavigation'
import {isImeKeydown} from '@/lib/imeKey'

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
  // Whether a keydown came from the composer this menu belongs to. The
  // listener is window-level (capture), so without this check the open menu
  // claimed Enter/Tab/arrows/Escape typed ANYWHERE in the app.
  isOwnTarget?: (target: EventTarget | null) => boolean
}

export default function FileAutocomplete({query, files, onSelect, onClose, onMatchCount, isOwnTarget}: Props) {
  const {t} = useTranslation('chat')

  const filtered = useMemo(() => {
    const q = query.toLowerCase()
    return files.filter(f => f.filename.toLowerCase().includes(q))
  }, [files, query])

  // Same keyboard contract as SlashCommandAutocomplete: arrows move, Enter/Tab
  // select, Escape closes. Before this the @-menu had no key handling at all
  // while ChatInput still swallowed every key it saw.
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
      // Accepting an IME candidate must not pick an entry.
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
      // Keep focus in the composer (a blur there closes the menu) wherever
      // the menu is clicked — heading and padding included, not only items.
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
