import {useState, useEffect} from 'react'
import clsx from 'clsx'
import {Plus, Trash2, Minimize2, MessageSquare, Wrench, Search} from 'lucide-react'

interface Props {
  messageCount: number
  onNewChat: () => void
  onClearContext: () => void
  onCompact: () => void
  useTools?: boolean
  onToggleTools?: () => void
  onToggleSearch?: () => void
  searchActive?: boolean
}

export default function ChatToolbar({
  messageCount,
  onNewChat,
  onClearContext,
  onCompact,
  useTools,
  onToggleTools,
  onToggleSearch,
  searchActive,
}: Props) {
  const [pendingAction, setPendingAction] = useState<'clear' | 'compact' | null>(null)

  useEffect(() => {
    if (!pendingAction) return
    const t = setTimeout(() => setPendingAction(null), 5000)
    return () => clearTimeout(t)
  }, [pendingAction])

  return (
    <div className="flex items-center gap-1 px-3 py-1">
      <button
        className="flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs text-text-tertiary hover:text-brand-400 hover:bg-brand-500/8 transition-colors"
        onClick={onNewChat}
        title="Start a new conversation in a new thread"
      >
        <Plus size={12} />
        <span>New chat</span>
      </button>

      {onToggleTools && (
        <button
          className={clsx(
            'flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs transition-colors',
            useTools
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-brand-400 hover:bg-brand-500/8',
          )}
          onClick={onToggleTools}
          aria-pressed={!!useTools}
          title="Let the assistant look up flow details on demand (Claude, OpenAI, xAI, GLM, GitHub Models). Off = single-pass answer."
        >
          <Wrench size={12} />
          <span>Tools{useTools ? ' on' : ''}</span>
        </button>
      )}

      {messageCount > 0 && !pendingAction && (
        <>
          <button
            className="flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs text-text-tertiary hover:text-amber-400 hover:bg-amber-500/8 transition-colors"
            onClick={() => setPendingAction('compact')}
            title="Keep only the last 3 exchanges to reduce token usage"
          >
            <Minimize2 size={12} />
            <span>Compact</span>
          </button>

          <button
            className="flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs text-text-tertiary hover:text-red-400 hover:bg-red-500/8 transition-colors"
            onClick={() => setPendingAction('clear')}
            title="Delete all messages in this thread"
          >
            <Trash2 size={12} />
            <span>Clear</span>
          </button>
        </>
      )}

      {pendingAction && (
        <div className="flex items-center gap-1.5 ml-1 animate-fade-in">
          <span className="text-2xs text-text-tertiary">
            {pendingAction === 'clear' ? 'Delete all messages?' : 'Keep last 3 exchanges?'}
          </span>
          <button
            onClick={() => setPendingAction(null)}
            className="text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={() => {
              if (pendingAction === 'clear') onClearContext()
              else onCompact()
              setPendingAction(null)
            }}
            className={clsx(
              'text-2xs px-1.5 py-0.5 rounded font-medium transition-colors',
              pendingAction === 'clear' ? 'text-red-400 hover:bg-red-500/10' : 'text-amber-400 hover:bg-amber-500/10',
            )}
          >
            Confirm
          </button>
        </div>
      )}

      <div className="ml-auto flex items-center gap-1">
        {messageCount > 0 && onToggleSearch && (
          <button
            onClick={onToggleSearch}
            className={clsx(
              'p-1 rounded transition-colors',
              searchActive
                ? 'text-brand-400 bg-brand-500/10'
                : 'text-text-tertiary/50 hover:text-text-tertiary hover:bg-surface-3',
            )}
            title="Search messages"
            aria-label="Search messages"
            aria-pressed={!!searchActive}
          >
            <Search size={11} />
          </button>
        )}
        {messageCount > 0 && (
          <span className="text-2xs text-text-tertiary/50 flex items-center gap-1">
            <MessageSquare size={10} />
            {messageCount}
          </span>
        )}
      </div>
    </div>
  )
}
