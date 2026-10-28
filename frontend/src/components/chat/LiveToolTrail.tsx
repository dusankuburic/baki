import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {ChevronDown, ChevronUp} from 'lucide-react'
import {useChatStore} from '@/stores/chatStore'
import type {ToolCallRecord} from '@/types'

const NO_CALLS: ToolCallRecord[] = []

// R-frontend-4: the trail subscribes to the active stream's toolStatus and toolCalls (no polling), so a tool shows
// the moment its event lands and nothing runs while no tool is active.
export default function LiveToolTrail() {
  const {t} = useTranslation('chat')
  const isStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const toolStatus = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolStatus ?? null) : null))
  const all = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolCalls ?? NO_CALLS) : NO_CALLS))
  const [expanded, setExpanded] = useState(false)
  const [prevStreaming, setPrevStreaming] = useState(isStreaming)

  if (prevStreaming !== isStreaming) {
    setPrevStreaming(isStreaming)
    if (!isStreaming) setExpanded(false)
  }

  const totalCalls = all.length
  const calls = expanded ? all : all.slice(-3)
  const hiddenCount = Math.max(0, totalCalls - calls.length)

  if (!isStreaming || (toolStatus == null && calls.length === 0)) return null

  return (
    <div
      className="px-1 pt-1.5 flex flex-col gap-0.5"
      role="status"
      aria-label={t('a11y.toolActivity')}
      data-testid="live-tool-trail"
    >
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
          aria-label={t('trail.showEarlier', {count: hiddenCount})}
          data-testid="tool-trail-expand"
        >
          <ChevronUp size={10} className="inline" /> {t('trail.earlierCalls', {count: hiddenCount})}
        </button>
      )}
      {expanded && totalCalls > 3 && (
        <button
          onClick={() => setExpanded(false)}
          className="self-start text-2xs text-text-tertiary hover:text-text-secondary px-1 -ml-1 rounded transition-colors"
          data-testid="tool-trail-collapse"
        >
          <ChevronDown size={10} className="inline" /> {t('trail.collapse')}
        </button>
      )}
      {calls.map((call, i) => (
        <div key={i} className="flex items-baseline gap-1.5 text-2xs">
          <span
            role="img"
            aria-label={call.ok ? t('trail.succeeded') : t('trail.failed')}
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
