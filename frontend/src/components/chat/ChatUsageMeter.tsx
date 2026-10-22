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

const TOKEN_REFRESH_MS = 250

export default function ChatUsageMeter({promptTokens, completionTokens}: Props) {
  const {t} = useTranslation('chat')
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
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

  return (
    <span className="text-2xs text-text-tertiary tabular-nums truncate">
      {t('tokens.total', {formatted: formatCompact(total)})}
      {promptTokens > 0 && completionTokens > 0 && (
        <span className="cq-usage-detail text-text-tertiary/60">
          {' '}
          ({formatCompact(promptTokens)}/{formatCompact(completionTokens)})
        </span>
      )}
    </span>
  )
}
