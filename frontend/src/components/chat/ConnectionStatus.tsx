import {useState, useEffect} from 'react'
import clsx from 'clsx'
import {subscribeConnectionState, getEventConnectionState, type EventConnectionState} from '@/api/client'

interface Props {
  provider?: string
  isStreaming?: boolean
}

type DisplayState = 'ready' | 'streaming' | 'connecting' | 'reconnecting'

function toDisplayState(sse: EventConnectionState, isStreaming?: boolean): DisplayState {
  if (sse === 'reconnecting') return 'reconnecting'
  if (sse === 'connecting') return 'connecting'
  if (sse === 'open') return isStreaming ? 'streaming' : 'ready'
  return 'ready' // 'idle' — SSE not active between streams; backend is up
}

export default function ConnectionStatus({provider, isStreaming}: Props) {
  const [sseState, setSseState] = useState<EventConnectionState>(getEventConnectionState)

  useEffect(() => subscribeConnectionState(setSseState), [])

  const display = toDisplayState(sseState, isStreaming)

  const labels: Record<DisplayState, string> = {
    ready: 'Ready',
    streaming: 'Streaming',
    connecting: 'Connecting...',
    reconnecting: 'Reconnecting...',
  }

  return (
    <div className="flex items-center gap-1.5 px-2 py-1 rounded-full bg-surface-2 border border-border-subtle">
      <span
        className={clsx(
          'w-1.5 h-1.5 rounded-full',
          display === 'ready' && 'bg-success shadow-[0_0_8px_var(--success)]',
          display === 'streaming' && 'bg-brand-400 animate-pulse-soft shadow-[0_0_8px_var(--brand-400)]',
          (display === 'connecting' || display === 'reconnecting') && 'bg-warning animate-pulse-soft',
        )}
      />
      <span className="text-2xs text-text-tertiary">
        {labels[display]}
      </span>
      {provider && (display === 'ready' || display === 'streaming') && (
        <span className="text-2xs text-text-tertiary/60">· {provider}</span>
      )}
    </div>
  )
}
