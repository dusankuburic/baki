import {Activity} from 'lucide-react'
import {formatCompact} from '@/lib/format'

interface Props {
  promptTokens: number
  completionTokens: number
  inputCostPerM?: number
  outputCostPerM?: number
}

export default function TokenCounter({promptTokens, completionTokens, inputCostPerM, outputCostPerM}: Props) {
  if (promptTokens === 0 && completionTokens === 0) return null

  const total = promptTokens + completionTokens
  const hasPricing = (inputCostPerM ?? 0) > 0 || (outputCostPerM ?? 0) > 0

  let costStr = ''
  if (hasPricing) {
    const cost = ((promptTokens * (inputCostPerM ?? 0)) + (completionTokens * (outputCostPerM ?? 0))) / 1_000_000
    costStr = cost < 0.001 ? '<$0.001' : `$${cost.toFixed(3)}`
  }

  return (
    <div className="flex items-center gap-1.5 px-3 py-1">
      <Activity size={10} className="text-text-tertiary/50" />
      <span className="text-2xs text-text-tertiary">
        {formatCompact(total)} tokens
        {promptTokens > 0 && completionTokens > 0 && (
          <span className="text-text-tertiary/50"> ({formatCompact(promptTokens)} in / {formatCompact(completionTokens)} out)</span>
        )}
        {costStr && <span className="text-text-tertiary/50"> · {costStr}</span>}
      </span>
    </div>
  )
}
