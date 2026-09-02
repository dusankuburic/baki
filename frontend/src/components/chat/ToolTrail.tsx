import {useId, useState} from 'react'
import {Wrench} from 'lucide-react'
import type {FixProposalSnapshot, ToolCallRecord} from '@/types'

function formatDuration(ms: number | undefined): string {
  if (ms == null || ms < 0) return ''
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

// ToolTrail renders the persisted tool-call record behind an assistant
// message: collapsed to a one-line summary ("Used 3 tools"), expandable to the
// per-call rows (name, duration, one-line outcome). Pure display — the records
// arrive from tool_result events and travel with the saved conversation.
export function ToolTrail({calls}: {calls: ToolCallRecord[]}) {
  const [open, setOpen] = useState(false)
  // useId BEFORE the early return — hooks must run unconditionally.
  const listId = useId()
  if (calls.length === 0) return null
  const failed = calls.filter(c => !c.ok).length

  return (
    <div className="px-1" data-testid="tool-trail">
      <button
        type="button"
        className="flex items-center gap-1.5 text-2xs text-text-tertiary hover:text-text-secondary transition-colors"
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        aria-controls={listId}
      >
        <Wrench size={10} />
        <span>
          Used {calls.length} {calls.length === 1 ? 'tool' : 'tools'}
          {failed > 0 && <span className="text-semantic-error/80"> · {failed} failed</span>}
        </span>
        <span className="text-text-tertiary/60">{open ? 'Hide' : 'Show'}</span>
      </button>
      {open && (
        <ul id={listId} className="mt-1 space-y-0.5">
          {calls.map((call, i) => (
            <li key={i} className="flex items-baseline gap-1.5 text-2xs leading-relaxed">
              <span
                role="img"
                aria-label={call.ok ? 'succeeded' : 'failed'}
                className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${call.ok ? 'bg-semantic-success/70' : 'bg-semantic-error/80'}`}
              />
              <span className="font-medium text-text-secondary">{call.label || call.name}</span>
              {call.durationMs != null && call.durationMs > 0 && (
                <span className="shrink-0 text-text-tertiary/60">{formatDuration(call.durationMs)}</span>
              )}
              {call.summary && <span className="min-w-0 truncate text-text-tertiary/80">{call.summary}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// Status copy + tone for a persisted fix outcome (mirrors FixProposalCard's
// live card, minus the interactive states).
const OUTCOME: Record<string, {text: string; dot: string}> = {
  cancelled: {text: 'cancelled — generation stopped before you decided', dot: 'bg-text-disabled'},
  applied: {text: 'Fix applied and verified', dot: 'bg-semantic-success/70'},
  'applied-unresolved': {text: 'Applied, but the finding still appears', dot: 'bg-amber-400/80'},
  declined: {text: 'Declined — nothing was changed', dot: 'bg-text-tertiary/60'},
  timeout: {text: 'No response in time — nothing was changed', dot: 'bg-amber-400/80'},
  error: {text: 'Fix failed', dot: 'bg-red-400/80'},
  applying: {text: 'Applying…', dot: 'bg-brand-400'},
  pending: {text: 'Awaiting decision', dot: 'bg-brand-400'},
}

const ITEM_OUTCOME: Record<string, {text: string; dot: string}> = {
  applied: {text: 'applied', dot: 'bg-green-500/70'},
  'applied-unresolved': {text: 'still appears', dot: 'bg-amber-400/80'},
  error: {text: 'failed', dot: 'bg-red-400/80'},
  'already-resolved': {text: 'already resolved', dot: 'bg-text-tertiary/60'},
}

// FixOutcomeStrip is the persisted record of an apply_fix / apply_fixes
// approval: what was proposed and how it resolved, attached to the assistant
// message so it survives the stream that produced it. Batches render one
// per-fix row list under the overall status.
export function FixOutcomeStrip({snapshot}: {snapshot: FixProposalSnapshot}) {
  const outcome = OUTCOME[snapshot.status] ?? {text: snapshot.status, dot: 'bg-text-tertiary/60'}
  const items = snapshot.items ?? []
  const batch = items.length > 1
  return (
    <div
      className="mx-1 rounded-md border border-border-subtle bg-surface-2 px-2 py-1 text-2xs"
      data-testid="fix-outcome"
    >
      <div className="flex items-baseline gap-1.5">
        <span className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${outcome.dot}`} aria-hidden="true" />
        <span className="font-medium text-text-secondary">
          {batch ? `${items.length} fixes` : <>Fix <code className="font-mono text-brand-300">{snapshot.fixType}</code></>}
          {!batch && snapshot.blockLabel && <span className="text-text-tertiary"> · {snapshot.blockLabel}</span>}
        </span>
        <span className="text-text-tertiary">{outcome.text}</span>
        {snapshot.message && <span className="min-w-0 truncate text-text-tertiary/70">— {snapshot.message}</span>}
      </div>
      {batch && (
        <ul className="mt-1 space-y-0.5">
          {items.map((it, i) => {
            const io = ITEM_OUTCOME[it.status] ?? {text: it.status, dot: 'bg-text-tertiary/60'}
            return (
              <li key={i} className="flex items-baseline gap-1.5">
                <span className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${io.dot}`} aria-hidden="true" />
                <span className="text-text-secondary">
                  {it.blockLabel} <span className="text-text-tertiary/60">· {io.text}</span>
                </span>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
