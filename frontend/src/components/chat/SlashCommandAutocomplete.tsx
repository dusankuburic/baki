import clsx from 'clsx'
import {Terminal, Info, Zap, Trash2, HelpCircle} from 'lucide-react'
import {useEffect} from 'react'
import {useListNavigation} from '@/hooks/useListNavigation'

export interface SlashCommand {
  id: string
  label: string
  description: string
  icon: React.ReactNode
  // 'insert' commands drop their text into the composer for the user to
  // extend and send; 'action' commands run a local handler (never sent to the
  // model) — the action key routes to the matching callback in ChatInput.
  kind: 'insert' | 'action'
  action?: 'clear' | 'help'
}

const COMMANDS: SlashCommand[] = [
  {id: '/explain', label: 'Explain', description: 'Explain how this code works in detail', icon: <Info size={14} />, kind: 'insert'},
  {id: '/fix', label: 'Fix', description: 'Find and fix bugs in this code', icon: <Zap size={14} />, kind: 'insert'},
  {id: '/test', label: 'Test', description: 'Generate unit tests for this code', icon: <Terminal size={14} />, kind: 'insert'},
  {id: '/clear', label: 'Clear', description: 'Clear the current conversation thread', icon: <Trash2 size={14} />, kind: 'action', action: 'clear'},
  {id: '/help', label: 'Help', description: 'Show available commands and shortcuts', icon: <HelpCircle size={14} />, kind: 'action', action: 'help'},
]

export const SLASH_COMMANDS = COMMANDS

interface Props {
  query: string
  onSelect: (command: SlashCommand) => void
  onClose: () => void
}

export default function SlashCommandAutocomplete({query, onSelect, onClose}: Props) {
  const filtered = COMMANDS.filter(c =>
    c.id.toLowerCase().includes(query.toLowerCase()) ||
    c.label.toLowerCase().includes(query.toLowerCase())
  )

  const {activeIndex: selectedIndex, setActiveIndex, handleKeyDown} = useListNavigation({
    count: filtered.length,
    onSelect: (i) => { if (filtered[i]) onSelect(filtered[i]) },
    onClose,
    mode: 'wrap',
    extraSelectKeys: ['Tab'],
  })

  useEffect(() => {
    setActiveIndex(0)
  }, [query, setActiveIndex])

  useEffect(() => {
    window.addEventListener('keydown', handleKeyDown as (e: KeyboardEvent) => void, true)
    return () => window.removeEventListener('keydown', handleKeyDown as (e: KeyboardEvent) => void, true)
  }, [handleKeyDown])

  if (filtered.length === 0) return null

  return (
    <div className="absolute bottom-full left-0 mb-2 w-64 bg-surface-2 border border-border-default rounded-lg shadow-xl overflow-hidden animate-in fade-in slide-in-from-bottom-2 duration-200">
      <div className="px-3 py-2 bg-surface-3 border-b border-border-subtle">
        <span className="text-2xs font-semibold text-text-tertiary uppercase tracking-wider">Commands</span>
      </div>
      <div className="max-h-60 overflow-y-auto p-1">
        {filtered.map((cmd, i) => (
          <button
            key={cmd.id}
            className={clsx(
              'w-full flex items-center gap-3 px-3 py-2 text-sm rounded-md transition-colors text-left',
              i === selectedIndex ? 'bg-brand-500/10 text-brand-400' : 'text-text-secondary hover:bg-surface-3'
            )}
            onClick={() => onSelect(cmd)}
          >
            <div className={clsx(
              'p-1.5 rounded-md',
              i === selectedIndex ? 'bg-brand-500/20 text-brand-400' : 'bg-surface-4 text-text-tertiary'
            )}>
              {cmd.icon}
            </div>
            <div className="flex flex-col min-w-0">
              <span className="font-medium truncate">{cmd.label}</span>
              <span className="text-xs text-text-tertiary truncate">{cmd.description}</span>
            </div>
          </button>
        ))}
      </div>
    </div>
  )
}
