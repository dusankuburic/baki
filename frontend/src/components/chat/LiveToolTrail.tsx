import {TRAIL_REFRESH_MS} from '@/lib/constants'
import {useEffect, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import type {ToolCallRecord} from '@/types'

// Same throttled-polling pattern as StreamingProgress: the slot's toolCalls
// array changes per tool execution (not per frame), but a store subscription
// here would re-render AITab's whole tree on every finished tool; polling
// keeps this leaf isolated and cheap.


// LiveToolTrail is the streaming view of the slot's finished tool executions:
// the current tool's pulsing label (from toolStatus) plus the last few
// finished calls with ok/fail dots. Before this, a multi-iteration tool loop
// showed ONE spinning label for its whole duration with zero history — the
// full trail only appeared after the message committed.
export default function LiveToolTrail() {
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const [toolStatus, setToolStatus] = useState<string | null>(null)
  const [calls, setCalls] = useState<ToolCallRecord[]>([])
  const [totalCalls, setTotalCalls] = useState(0)
  // V1.3: the collapsed trail shows the last 3 finished calls; "Show all"
  // expands the full history while still streaming (it used to land only on
  // the committed message).
  const [expanded, setExpanded] = useState(false)

  useEffect(() => {
    if (!isStreaming) {
      setToolStatus(null)
      setCalls([])
      setTotalCalls(0)
      setExpanded(false)
      return
    }
    const read = () => {
      const st = useChatStore.getState()
      const slot = st.activeThreadId ? st.streams[st.activeThreadId] : undefined
      setToolStatus(slot?.toolStatus ?? null)
      const all = slot ? slot.toolCalls : []
      setTotalCalls(all.length)
      setCalls(expanded ? all : all.slice(-3))
    }
    read()
    const id = setInterval(read, TRAIL_REFRESH_MS)
    return () => clearInterval(id)
  }, [isStreaming, expanded])

  const hiddenCount = Math.max(0, totalCalls - calls.length)

  if (!isStreaming || (toolStatus == null && calls.length === 0)) return null

  return (
    <div className="px-3 pt-1.5 flex flex-col gap-0.5" role="status" aria-label="AI tool activity" data-testid="live-tool-trail">
      {toolStatus != null && (
        <div className="flex items-center gap-2 text-2xs text-brand-400">
          <span className="inline-block w-1.5 h-1.5 rounded-full bg-brand-400 animate-pulse" aria-hidden="true" />
          <span className="truncate">{toolStatus}…</span>
        </div>
      )}
      {hiddenCount > 0 && (
        <button
          onClick={() => setExpanded(true)}
          className="self-start text-2xs text-text-tertiary hover:text-text-secondary px-1 -ml-1 rounded transition-colors"
          aria-label={`Show ${hiddenCount} earlier tool calls`}
          data-testid="tool-trail-expand"
        >
          ⌃ {hiddenCount} earlier call{hiddenCount !== 1 ? 's' : ''}
        </button>
      )}
      {expanded && totalCalls > 3 && (
        <button
          onClick={() => setExpanded(false)}
          className="self-start text-2xs text-text-tertiary hover:text-text-secondary px-1 -ml-1 rounded transition-colors"
          data-testid="tool-trail-collapse"
        >
          ⌄ collapse
        </button>
      )}
      {calls.map((call, i) => (
        <div key={i} className="flex items-baseline gap-1.5 text-2xs">
          <span
            role="img"
            aria-label={call.ok ? 'succeeded' : 'failed'}
            className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${call.ok ? 'bg-semantic-success/70' : 'bg-semantic-error/80'}`}
          />
          <span className="text-text-tertiary/80 truncate">
            {call.label || call.name}
            {call.durationMs != null && call.durationMs > 0 && (
              <span className="text-text-tertiary/50"> {(call.durationMs / 1000).toFixed(1)}s</span>
            )}
          </span>
        </div>
      ))}
    </div>
  )
}
