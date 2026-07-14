import clsx from 'clsx'
import type {Finding} from '@/types'

interface Props {
  related: Finding[] | null
  relatedLoading: boolean
  relatedError: boolean
  onRetry: () => void
}

export default function FindingRelatedPanel({related, relatedLoading, relatedError, onRetry}: Props) {
  return (
    <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-surface-3 border border-border-subtle rounded space-y-1">
      <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">Related findings</span>
      {relatedLoading ? (
        <span className="text-2xs text-text-tertiary">Loading…</span>
      ) : relatedError ? (
        <div className="flex items-center gap-2 text-2xs text-text-tertiary">
          <span>Couldn't load related findings.</span>
          <button onClick={onRetry} className="text-brand-400 hover:text-brand-300 font-medium">
            Retry
          </button>
        </div>
      ) : related && related.length > 0 ? (
        related.map(r => (
          <div key={r.id} className="flex items-center gap-2 text-2xs">
            <span
              className={clsx(
                'font-bold uppercase',
                r.severity === 'error' ? 'text-red-400' : r.severity === 'warning' ? 'text-amber-400' : 'text-blue-400',
              )}
            >
              {r.severity}
            </span>
            <span className="text-text-secondary truncate">{r.title}</span>
            <span className="text-text-tertiary shrink-0">{r.ruleId}</span>
          </div>
        ))
      ) : (
        <span className="text-2xs text-text-tertiary">No other findings for this block.</span>
      )}
    </div>
  )
}
