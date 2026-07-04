import {useEffect, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {formatCompact} from '@/lib/format'

// Token-count refresh cadence. The store updates the slot's token estimate at
// ~60fps (RAF-coalesced with the streaming text), but a numeric counter only
// needs ~4fps to feel live — so StreamingProgress polls the store on an
// interval instead of subscribing reactively. This keeps AITab (which used to
// receive streamingTokens as a prop) from re-rendering every frame during
// generation; only this small leaf component repaints, 4×/second.
const TOKEN_REFRESH_MS = 250

export default function StreamingProgress() {
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const [tokens, setTokens] = useState(0)

  useEffect(() => {
    if (!isStreaming) return
    // Seed immediately so the counter doesn't lag a tick on stream start, then
    // poll on a throttle. The store updates at ~60fps but the numeric display
    // only needs ~4fps to feel live; this keeps AITab off the per-frame path.
    const read = () => {
      const st = useChatStore.getState()
      setTokens(st.activeThreadId ? (st.streams[st.activeThreadId]?.tokens || 0) : 0)
    }
    read()
    const id = setInterval(read, TOKEN_REFRESH_MS)
    return () => clearInterval(id)
  }, [isStreaming])

  if (!isStreaming || tokens === 0) return null

  return (
    <div className="px-3 pb-2 flex items-center gap-2">
      <div className="flex-1 h-0.5 bg-surface-3 rounded-full overflow-hidden">
        <div
          className="h-full w-1/3 bg-gradient-to-r from-brand-500 to-brand-400 animate-pulse"
          style={{backgroundSize: '200% 100%'}}
        />
      </div>
      <span className="text-2xs text-text-tertiary tabular-nums">{formatCompact(tokens)} tokens</span>
    </div>
  )
}
