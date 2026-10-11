import {useId} from 'react'
import clsx from 'clsx'

interface Props {
  prompts: string[]
  onSelect: (prompt: string) => void
  disabledReason?: string
}

export default function SuggestedPrompts({prompts, onSelect, disabledReason}: Props) {
  const reasonId = useId()
  if (prompts.length === 0) return null

  return (
    <div className="chat-prompt-strip chat-gutter flex gap-2 py-2 overflow-x-auto scrollbar-none">
      {disabledReason && (
        <span id={reasonId} className="sr-only">
          {disabledReason}
        </span>
      )}
      {prompts.map(p => (
        <button
          key={p}
          type="button"
          aria-disabled={disabledReason ? true : undefined}
          aria-describedby={disabledReason ? reasonId : undefined}
          title={disabledReason}
          className={clsx(
            'bg-surface-2 text-xs px-3 py-1.5 rounded-full whitespace-nowrap transition-colors duration-fast border border-border-default',
            disabledReason ? 'opacity-50 cursor-not-allowed' : 'hover:bg-surface-3',
          )}
          onClick={() => {
            if (!disabledReason) onSelect(p)
          }}
        >
          {p}
        </button>
      ))}
    </div>
  )
}
