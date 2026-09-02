import {useEffect} from 'react'
import {X} from 'lucide-react'
import {SLASH_COMMANDS} from './SlashCommandAutocomplete'

interface Props {
  onClose: () => void
}

const SHORTCUTS: {keys: string; description: string}[] = [
  {keys: 'Enter', description: 'Send message'},
  {keys: 'Shift/Cmd/Ctrl + Enter', description: 'New line'},
  {keys: '↑ / ↓', description: 'Cycle sent-message history (empty input)'},
  {keys: '@', description: 'Mention a source file'},
  {keys: '/', description: 'Open the command menu'},
]

// ChatHelpPopover lists the slash commands and keyboard shortcuts. Backs the
// /help slash command; a plain modal-style overlay so it works regardless of
// where the composer sits (docked or popped out).
export default function ChatHelpPopover({onClose}: Props) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div
      className="fixed inset-0 z-overlay flex items-center justify-center bg-black/40 p-4"
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label="Chat help"
    >
      <div
        className="w-full max-w-sm bg-surface-2 border border-border-default rounded-xl shadow-xl overflow-hidden"
        onClick={e => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 py-3 border-b border-border-subtle">
          <h2 className="text-sm font-semibold text-text-primary">Chat commands & shortcuts</h2>
          <button
            className="p-1 rounded hover:bg-surface-3 text-text-tertiary hover:text-text-secondary"
            onClick={onClose}
            aria-label="Close help"
          >
            <X size={14} />
          </button>
        </div>

        <div className="p-4 space-y-4">
          <div>
            <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">Commands</p>
            <ul className="space-y-1.5">
              {SLASH_COMMANDS.map(c => (
                <li key={c.id} className="flex items-baseline gap-2 text-sm">
                  <code className="text-brand-400 font-medium">{c.id}</code>
                  <span className="text-text-tertiary text-xs">{c.description}</span>
                </li>
              ))}
            </ul>
          </div>

          <div>
            <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">Shortcuts</p>
            <ul className="space-y-1.5">
              {SHORTCUTS.map(s => (
                <li key={s.keys} className="flex items-baseline justify-between gap-3 text-sm">
                  <span className="text-text-secondary text-xs">{s.description}</span>
                  <kbd className="text-2xs text-text-tertiary bg-surface-3 border border-border-subtle rounded px-1.5 py-0.5 whitespace-nowrap">
                    {s.keys}
                  </kbd>
                </li>
              ))}
            </ul>
          </div>

          <div>
            <p className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-2">AI tools &amp; fixes</p>
            <ul className="space-y-1.5 text-xs text-text-tertiary">
              <li>
                <span className="text-text-secondary font-medium">Tools toggle</span> — when on, the assistant can look
                up flow details (findings, blocks, source) on demand. Each finished lookup shows in the message's tool
                trail after it completes.
              </li>
              <li>
                <span className="text-text-secondary font-medium">Fix approval</span> — when the assistant proposes a
                source fix, an approval card appears. Nothing is written until you click <em>Approve &amp; apply</em>;
                the proposal expires (declined, nothing changed) if you don't decide within a minute. <kbd className="text-2xs text-text-tertiary bg-surface-3 border border-border-subtle rounded px-1.5 py-0.5">Esc</kbd> dismisses.
              </li>
            </ul>
          </div>
        </div>
      </div>
    </div>
  )
}
