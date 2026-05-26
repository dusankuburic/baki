interface Props {
  prompts: string[]
  onSelect: (prompt: string) => void
}

export default function SuggestedPrompts({prompts, onSelect}: Props) {
  if (prompts.length === 0) return null

  return (
    <div className="flex gap-2 px-3 py-2 overflow-x-auto scrollbar-none">
      {prompts.map(p => (
        <button
          key={p}
          className="bg-surface-2 hover:bg-surface-3 text-xs px-3 py-1.5 rounded-full whitespace-nowrap transition-colors duration-fast border border-border-default"
          onClick={() => onSelect(p)}
        >
          {p}
        </button>
      ))}
    </div>
  )
}
