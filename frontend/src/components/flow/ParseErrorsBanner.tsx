import {useState} from 'react'
import clsx from 'clsx'
import {AlertTriangle, ChevronDown, X} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'

// ParseErrorsBanner surfaces the per-line issues the parser recovered from
// while loading the current flow. Previously these were silently dropped, so
// a partially-parsed flow looked identical to a clean one.
export default function ParseErrorsBanner() {
  const document = useFlowStore(s => s.document)
  const [expanded, setExpanded] = useState(false)
  const [dismissed, setDismissed] = useState(false)
  const [prevDocId, setPrevDocId] = useState(document?.id)

  // Reset banner state when a different document is loaded.
  // Uses the "store info from previous render" pattern recommended by React
  // instead of useEffect+setState (which causes cascading renders).
  if (document?.id !== prevDocId) {
    setPrevDocId(document?.id)
    setDismissed(false)
    setExpanded(false)
  }

  const errors = document?.parseErrors
  if (!errors || errors.length === 0 || dismissed) return null

  const errorCount = errors.filter(e => e.severity === 'error').length
  const warnCount = errors.length - errorCount

  return (
    <div className="border-b border-amber-500/20 bg-amber-500/5 flex-shrink-0">
      <div className="px-3 py-1.5 flex items-center gap-2">
        <AlertTriangle size={13} className="text-amber-400 shrink-0" />
        <button
          onClick={() => setExpanded(e => !e)}
          aria-expanded={expanded}
          className="flex items-center gap-1.5 text-2xs text-amber-300 hover:text-amber-200 transition-colors"
        >
          <span className="font-medium">
            {errors.length} parse issue{errors.length !== 1 ? 's' : ''} in this flow
          </span>
          <span className="text-amber-400/70">
            ({errorCount} error{errorCount !== 1 ? 's' : ''}, {warnCount} warning{warnCount !== 1 ? 's' : ''})
          </span>
          <ChevronDown size={11} className={clsx('transition-transform duration-fast', expanded && 'rotate-180')} />
        </button>
        <span className="text-2xs text-text-tertiary">— affected lines were skipped or partially parsed</span>
        <button
          onClick={() => setDismissed(true)}
          aria-label="Dismiss parse issues banner"
          className="ml-auto text-text-tertiary hover:text-text-secondary p-0.5 rounded hover:bg-surface-3 transition-colors shrink-0"
        >
          <X size={12} />
        </button>
      </div>

      {expanded && (
        <div className="px-3 pb-2 max-h-40 overflow-y-auto space-y-1">
          {errors.slice(0, 50).map((e, i) => (
            <div
              key={`${e.line}-${i}`}
              className="flex items-start gap-2 px-2 py-1 rounded bg-surface-0/60 border border-border-subtle"
            >
              <span className={clsx(
                'text-2xs font-mono tabular-nums shrink-0 mt-px',
                e.severity === 'error' ? 'text-red-400' : 'text-amber-400',
              )}>
                L{e.line}{e.column ? `:${e.column}` : ''}
              </span>
              <div className="min-w-0">
                <span className="text-2xs text-text-secondary">{e.message}</span>
                {e.snippet && (
                  <pre className="text-2xs text-text-tertiary font-mono truncate mt-0.5">{e.snippet}</pre>
                )}
              </div>
            </div>
          ))}
          {errors.length > 50 && (
            <div className="text-2xs text-text-tertiary px-2">…and {errors.length - 50} more</div>
          )}
        </div>
      )}
    </div>
  )
}
