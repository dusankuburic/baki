import {useTranslation} from 'react-i18next'
import {useEffect, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {formatCompact} from '@/lib/format'

interface Props {
  promptTokens: number
  completionTokens: number
  inputCostPerM?: number
  outputCostPerM?: number
}

// Token-count refresh cadence. The store updates the slot's token estimate at
// ~60fps (RAF-coalesced with the streaming text), but a numeric counter only
// needs ~4fps to feel live — so this polls the store on an interval instead of
// subscribing reactively, keeping AITab off the per-frame path.
const TOKEN_REFRESH_MS = 250

// ChatUsageMeter is the composer's single always-present usage line. It merges
// what used to be two components that swapped places above the composer
// (StreamingProgress while generating, TokenCounter when idle) — each of which
// also returned null at zero, so a row appeared and vanished at the start and
// end of every stream and shoved the conversation up and down.
export default function ChatUsageMeter({promptTokens, completionTokens, inputCostPerM, outputCostPerM}: Props) {
  const {t} = useTranslation('chat')
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
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
        <span className="text-[10px] text-text-tertiary tabular-nums truncate" title={t('tokens.streamingTitle')}>
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

  const hasPricing = (inputCostPerM ?? 0) > 0 || (outputCostPerM ?? 0) > 0
  let costStr = ''
  if (hasPricing) {
    const cost = (promptTokens * (inputCostPerM ?? 0) + completionTokens * (outputCostPerM ?? 0)) / 1_000_000
    costStr = cost < 0.001 ? '<$0.001' : `$${cost.toFixed(3)}`
  }

  return (
    <span className="text-[10px] text-text-tertiary tabular-nums truncate">
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
