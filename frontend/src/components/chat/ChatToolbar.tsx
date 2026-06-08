import clsx from 'clsx'
import {Plus, Trash2, Minimize2, MessageSquare, Wrench} from 'lucide-react'

interface Props {
  messageCount: number
  onNewChat: () => void
  onClearContext: () => void
  onCompact: () => void
  useTools?: boolean
  onToggleTools?: () => void
}

export default function ChatToolbar({messageCount, onNewChat, onClearContext, onCompact, useTools, onToggleTools}: Props) {
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
              : 'text-text-tertiary hover:text-brand-400 hover:bg-brand-500/8'
          )}
          onClick={onToggleTools}
          aria-pressed={!!useTools}
          title="Let the assistant look up flow details on demand (Claude, OpenAI, xAI, GLM, GitHub Models). Off = single-pass answer."
        >
          <Wrench size={12} />
          <span>Tools{useTools ? ' on' : ''}</span>
        </button>
      )}

      {messageCount > 0 && (
        <>
          <button
            className="flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs text-text-tertiary hover:text-amber-400 hover:bg-amber-500/8 transition-colors"
            onClick={onCompact}
            title="Keep only the last 3 exchanges to reduce token usage"
          >
            <Minimize2 size={12} />
            <span>Compact</span>
          </button>

          <button
            className="flex items-center gap-1.5 px-2 py-1 rounded-md text-2xs text-text-tertiary hover:text-red-400 hover:bg-red-500/8 transition-colors"
            onClick={onClearContext}
            title="Delete all messages in this thread"
          >
            <Trash2 size={12} />
            <span>Clear</span>
          </button>
        </>
      )}

      {messageCount > 0 && (
        <span className="ml-auto text-2xs text-text-tertiary/50 flex items-center gap-1">
          <MessageSquare size={10} />
          {messageCount}
        </span>
      )}
    </div>
  )
}
