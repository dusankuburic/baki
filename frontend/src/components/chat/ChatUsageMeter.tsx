import {useTranslation} from 'react-i18next'
import {useEffect, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {formatCompact} from '@/lib/format'
import type {ChatMessage} from '@/types'

interface Props {
  promptTokens: number
  completionTokens: number
  inputCostPerM?: number
  outputCostPerM?: number
}

const TOKEN_REFRESH_MS = 250

const EMPTY_MESSAGES: readonly ChatMessage[] = []

export function threadCost(
  messages: readonly ChatMessage[],
  promptTokens: number,
  completionTokens: number,
  listInputPerM: number,
  listOutputPerM: number,
): number | null {
  return threadCostEstimate(messages, promptTokens, completionTokens, listInputPerM, listOutputPerM)?.cost ?? null
}

export function threadCostEstimate(
  messages: readonly ChatMessage[],
  promptTokens: number,
  completionTokens: number,
  listInputPerM: number,
  listOutputPerM: number,
): {cost: number; lowerBound: boolean} | null {
  let cost = 0
  let priced = false
  let ratedIn = 0
  let ratedOut = 0
  for (const m of messages) {
    if (m.role !== 'assistant') continue
    if (m.provider === 'azure-foundry') {
      ratedIn += m.tokensIn ?? 0
      ratedOut += m.tokensOut ?? 0
      continue
    }
    const inRate = Math.max(0, m.inputCostPerM ?? 0)
    const outRate = Math.max(0, m.outputCostPerM ?? 0)
    if (inRate === 0 && outRate === 0) continue
    const tokensIn = m.tokensIn ?? 0
    const tokensOut = m.tokensOut ?? 0
    cost += (tokensIn * inRate + tokensOut * outRate) / 1_000_000
    ratedIn += tokensIn
    ratedOut += tokensOut
    priced = true
  }
  const restIn = Math.max(0, promptTokens - ratedIn)
  const restOut = Math.max(0, completionTokens - ratedOut)
  if (listInputPerM > 0 || listOutputPerM > 0) {
    if (!priced && restIn + restOut === 0) return null
    cost += (restIn * listInputPerM + restOut * listOutputPerM) / 1_000_000
    return {cost, lowerBound: false}
  }
  return priced ? {cost, lowerBound: restIn + restOut > 0} : null
}

export default function ChatUsageMeter({promptTokens, completionTokens, inputCostPerM, outputCostPerM}: Props) {
  const {t} = useTranslation('chat')
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const messages = useChatStore(
    s => (s.activeThreadId ? s.conversations.get(s.activeThreadId) : undefined) ?? EMPTY_MESSAGES,
  )
  const showCost = useSettingsStore(s => s.settings.ai.showCostEstimates)
  const [live, setLive] = useState(0)

  useEffect(() => {
    if (!isStreaming) return
    const read = () => {
      const st = useChatStore.getState()
      setLive(st.activeThreadId ? st.streams[st.activeThreadId]?.tokens || 0 : 0)
    }
    read()
    const id = setInterval(read, TOKEN_REFRESH_MS)
    return () => clearInterval(id)
  }, [isStreaming])

  if (isStreaming) {
    return (
      <div
        className="flex items-center gap-2 min-w-0"
        role="status"
        aria-label={t('tokens.streamingAria', {count: live})}
      >
        <span className="w-10 h-0.5 rounded-full bg-surface-4 overflow-hidden shrink-0" aria-hidden="true">
          <span className="block w-1/3 h-full rounded-full bg-brand-400 animate-indeterminate" />
        </span>
        <span className="text-2xs text-text-tertiary tabular-nums truncate" title={t('tokens.streamingTitle')}>
          {t('tokens.streamingSoFar', {count: live, formatted: formatCompact(live)})}
        </span>
      </div>
    )
  }

  const total = promptTokens + completionTokens
  if (total === 0) {
    return <span className="block h-3.5" aria-hidden="true" />
  }

  const estimate = showCost
    ? threadCostEstimate(messages, promptTokens, completionTokens, inputCostPerM ?? 0, outputCostPerM ?? 0)
    : null
  const amount = estimate === null ? '' : estimate.cost < 0.001 ? '<$0.001' : `$${estimate.cost.toFixed(3)}`
  const costStr = estimate?.lowerBound && estimate.cost >= 0.001 ? t('tokens.costAtLeast', {cost: amount}) : amount

  return (
    <span className="text-2xs text-text-tertiary tabular-nums truncate">
      {t('tokens.total', {formatted: formatCompact(total)})}
      {promptTokens > 0 && completionTokens > 0 && (
        <span className="cq-usage-detail text-text-tertiary/60">
          {' '}
          ({formatCompact(promptTokens)}/{formatCompact(completionTokens)})
        </span>
      )}
      {costStr && (
        <span className="text-text-tertiary/60" title={estimate?.lowerBound ? t('tokens.costAtLeastTitle') : undefined}>
          {' · '}
          {costStr}
        </span>
      )}
    </span>
  )
}
