interface Props {
  tokens: number
  isStreaming: boolean
  estimatedTokens?: number
}

export default function StreamingProgress({tokens, isStreaming, estimatedTokens}: Props) {
  if (!isStreaming) return null

  const progress = estimatedTokens ? (tokens / estimatedTokens) * 100 : undefined

  return (
    <div className="px-3 pb-2 flex items-center gap-2">
      <div className="flex-1 h-0.5 bg-surface-3 rounded-full overflow-hidden">
        <div
          className="h-full bg-gradient-to-r from-brand-500 to-brand-400 transition-all duration-300 animate-shimmer"
          style={{
            width: progress ? `${Math.min(progress, 100)}%` : '100%',
            backgroundSize: '200% 100%'
          }}
        />
      </div>
      <span className="text-2xs text-text-tertiary">{tokens} tokens</span>
    </div>
  )
}
