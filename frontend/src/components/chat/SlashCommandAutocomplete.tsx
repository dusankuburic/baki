import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Terminal, Info, Zap, Trash2, HelpCircle} from 'lucide-react'
import {useEffect} from 'react'
import {useListNavigation} from '@/hooks/useListNavigation'
import {isImeKeydown} from '@/lib/imeKey'

export interface SlashCommand {
  id: string
  copyKey: 'explain' | 'fix' | 'test' | 'clear' | 'help'
  icon: React.ReactNode
  // 'insert' commands drop their text into the composer for the user to
  // extend and send; 'action' commands run a local handler (never sent to the
  // model) — the action key routes to the matching callback in ChatInput.
  kind: 'insert' | 'action'
  action?: 'clear' | 'help'
}

// Copy lives in chat:commands.{labels,descriptions}.<key>; this table holds the
// id, icon and behaviour. Keeping it a module constant (rather than a hook) means
// ChatHelpPopover can keep importing it directly.
const COMMANDS: SlashCommand[] = [
  {id: '/explain', copyKey: 'explain', icon: <Info size={14} />, kind: 'insert'},
  {id: '/fix', copyKey: 'fix', icon: <Zap size={14} />, kind: 'insert'},
  {id: '/test', copyKey: 'test', icon: <Terminal size={14} />, kind: 'insert'},
  {id: '/clear', copyKey: 'clear', icon: <Trash2 size={14} />, kind: 'action', action: 'clear'},
  {id: '/help', copyKey: 'help', icon: <HelpCircle size={14} />, kind: 'action', action: 'help'},
]

export const SLASH_COMMANDS = COMMANDS

interface Props {
  query: string
  onSelect: (command: SlashCommand) => void
  onClose: () => void
  // Reports how many entries the menu is showing so the composer knows whether
  // its keys belong to this menu — 0 means Enter must fall through and send.
  onMatchCount?: (n: number) => void
  // Whether a keydown came from the composer this menu belongs to. The
  // listener is window-level (capture): without this check, typing "/c" and
  // then pressing Tab or Enter anywhere else in the app ran /clear.
  isOwnTarget?: (target: EventTarget | null) => boolean
  // /clear is only offered where the thread can be cleared (not read-only).
  canClear?: boolean
}

export default function SlashCommandAutocomplete({
  query,
  onSelect,
  onClose,
  onMatchCount,
  isOwnTarget,
  canClear = true,
}: Props) {
  const {t} = useTranslation('chat')
  const q = query.toLowerCase()
  const filtered = COMMANDS.filter(
    c =>
      (canClear || c.action !== 'clear') &&
      (c.id.toLowerCase().includes(q) || t(`commands.labels.${c.copyKey}`).toLowerCase().includes(q)),
  )

  const {
    activeIndex: selectedIndex,
    setActiveIndex,
    handleKeyDown,
  } = useListNavigation({
    count: filtered.length,
    onSelect: i => {
      if (filtered[i]) onSelect(filtered[i])
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

  // Only claim the keyboard while there is something to pick. With no matches
  // the menu renders nothing, so Enter belongs to the composer.
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
        <span className="text-2xs font-semibold text-text-tertiary uppercase tracking-wider">
          {t('commands.heading')}
        </span>
      </div>
      <div className="max-h-60 overflow-y-auto p-1">
        {filtered.map((cmd, i) => (
          <button
            key={cmd.id}
            className={clsx(
              'w-full flex items-center gap-3 px-3 py-2 text-sm rounded-md transition-colors text-left',
              i === selectedIndex ? 'bg-brand-500/10 text-brand-400' : 'text-text-secondary hover:bg-surface-3',
            )}
            onClick={() => onSelect(cmd)}
          >
            <div
              className={clsx(
                'p-1.5 rounded-md',
                i === selectedIndex ? 'bg-brand-500/20 text-brand-400' : 'bg-surface-4 text-text-tertiary',
              )}
            >
              {cmd.icon}
            </div>
            <div className="flex flex-col min-w-0">
              <span className="font-medium truncate">{t(`commands.labels.${cmd.copyKey}`)}</span>
              <span className="text-xs text-text-tertiary truncate">{t(`commands.descriptions.${cmd.copyKey}`)}</span>
            </div>
          </button>
        ))}
      </div>
    </div>
  )
}
