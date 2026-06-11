import clsx from 'clsx'
import {Terminal, Info, Zap, Trash2, HelpCircle} from 'lucide-react'
import {useState, useEffect, useCallback} from 'react'

export interface SlashCommand {
  id: string
  label: string
  description: string
  icon: React.ReactNode
}

const COMMANDS: SlashCommand[] = [
  {id: '/explain', label: 'Explain', description: 'Explain how this code works in detail', icon: <Info size={14} />},
  {id: '/fix', label: 'Fix', description: 'Find and fix bugs in this code', icon: <Zap size={14} />},
  {id: '/test', label: 'Test', description: 'Generate unit tests for this code', icon: <Terminal size={14} />},
  {id: '/clear', label: 'Clear', description: 'Clear the current conversation thread', icon: <Trash2 size={14} />},
  {id: '/help', label: 'Help', description: 'Show available commands and shortcuts', icon: <HelpCircle size={14} />},
]

interface Props {
  query: string
  onSelect: (commandId: string) => void
  onClose: () => void
}

export default function SlashCommandAutocomplete({query, onSelect, onClose}: Props) {
  const [selectedIndex, setSelectedIndex] = useState(0)

  const filtered = COMMANDS.filter(c =>
    c.id.toLowerCase().includes(query.toLowerCase()) ||
    c.label.toLowerCase().includes(query.toLowerCase())
  )

  useEffect(() => {
    setSelectedIndex(0)
  }, [query])

  const handleKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      setSelectedIndex(i => (i + 1) % filtered.length)
      e.preventDefault()
    } else if (e.key === 'ArrowUp') {
      setSelectedIndex(i => (i - 1 + filtered.length) % filtered.length)
      e.preventDefault()
    } else if (e.key === 'Enter' || e.key === 'Tab') {
      if (filtered[selectedIndex]) {
        onSelect(filtered[selectedIndex].id)
        e.preventDefault()
      }
    } else if (e.key === 'Escape') {
      onClose()
      e.preventDefault()
    }
  }, [filtered, selectedIndex, onSelect, onClose])

  useEffect(() => {
    window.addEventListener('keydown', handleKeyDown, true)
    return () => window.removeEventListener('keydown', handleKeyDown, true)
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
            onClick={() => onSelect(cmd.id)}
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
