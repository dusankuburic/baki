import {useTranslation} from 'react-i18next'
import {useEffect, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {formatCompact} from '@/lib/format'
import type {ChatMessage} from '@/types'

interface Props {
  promptTokens: number
  completionTokens: number
  // The current model's listing price (USD per 1M tokens); 0 or undefined
  // when unknown. Prices the tokens no message carries its own rates for.
  inputCostPerM?: number
  outputCostPerM?: number
}

// Token-count refresh cadence. The store updates the slot's token estimate at
// ~60fps (RAF-coalesced with the streaming text), but a numeric counter only
// needs ~4fps to feel live — so this polls the store on an interval instead of
// subscribing reactively, keeping AITab off the per-frame path.
const TOKEN_REFRESH_MS = 250

const EMPTY_MESSAGES: readonly ChatMessage[] = []

// threadCost estimates the thread's spend in USD. Turns whose done event
// reported the rates they were billed at (an alias provider answering through
// a deployment, model-router or agent) are priced at those; the rest of the
// thread's totals at the current model's listing. Null when nothing can be
// priced — an unknown or zero rate hides the figure rather than showing $0.
export function threadCost(
  messages: readonly ChatMessage[],
  promptTokens: number,
  completionTokens: number,
  listInputPerM: number,
  listOutputPerM: number,
): number | null {
  let cost = 0
  let priced = false
  let ratedIn = 0
  let ratedOut = 0
  for (const m of messages) {
    if (m.role !== 'assistant') continue
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
  if (listInputPerM > 0 || listOutputPerM > 0) {
    const restIn = Math.max(0, promptTokens - ratedIn)
    const restOut = Math.max(0, completionTokens - ratedOut)
    cost += (restIn * listInputPerM + restOut * listOutputPerM) / 1_000_000
    priced = true
  }
  return priced ? cost : null
}

// ChatUsageMeter is the composer's single always-present usage line. It merges
// what used to be two components that swapped places above the composer
// (StreamingProgress while generating, TokenCounter when idle) — each of which
// also returned null at zero, so a row appeared and vanished at the start and
// end of every stream and shoved the conversation up and down.
export default function ChatUsageMeter({promptTokens, completionTokens, inputCostPerM, outputCostPerM}: Props) {
  const {t} = useTranslation('chat')
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  // The active thread's messages price the turns that carry their own billed
  // rates. They change per turn, not per frame, so subscribing is cheap.
  const messages = useChatStore(
    s => (s.activeThreadId ? s.conversations.get(s.activeThreadId) : undefined) ?? EMPTY_MESSAGES,
  )
  const showCost = useSettingsStore(s => s.settings.ai.showCostEstimates)
  const [live, setLive] = useState(0)

  useEffect(() => {
    if (!isStreaming) {
      // Clears a poll mirror when the stream ends.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setLive(0)
      return
    }
    // Seed immediately so the counter doesn't lag a tick on stream start.
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
        {/* A real travelling bar. The old one was a static one-third-width div
            with animate-pulse, which reads as a progress bar that is stuck. */}
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
    // Occupy the row's height without announcing anything, so the composer
    // does not resize when the first answer lands.
    return <span className="block h-3.5" aria-hidden="true" />
  }

  const cost = showCost
    ? threadCost(messages, promptTokens, completionTokens, inputCostPerM ?? 0, outputCostPerM ?? 0)
    : null
  const costStr = cost === null ? '' : cost < 0.001 ? '<$0.001' : `$${cost.toFixed(3)}`

  return (
    <span className="text-2xs text-text-tertiary tabular-nums truncate">
      {t('tokens.total', {formatted: formatCompact(total)})}
      {promptTokens > 0 && completionTokens > 0 && (
        <span className="cq-usage-detail text-text-tertiary/60">
          {' '}
          ({formatCompact(promptTokens)}/{formatCompact(completionTokens)})
        </span>
      )}
      {costStr && <span className="text-text-tertiary/60"> · {costStr}</span>}
    </span>
  )
}
