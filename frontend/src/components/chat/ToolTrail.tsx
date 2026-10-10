import {useTranslation} from 'react-i18next'
import {useId, useState} from 'react'
import {Wrench} from 'lucide-react'
import type {ApprovalSnapshot, FixProposalSnapshot, ToolCallRecord} from '@/types'

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
  const {t} = useTranslation('chat')
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
          {t('trail.usedTools', {count: calls.length})}
          {failed > 0 && <span className="text-semantic-error/80"> · {t('trail.nFailed', {count: failed})}</span>}
        </span>
        <span className="text-text-tertiary/60">{open ? t('trail.hide') : t('trail.show')}</span>
      </button>
      {open && (
        <ul id={listId} className="mt-1 space-y-0.5">
          {calls.map((call, i) => (
            <li key={i} className="flex items-baseline gap-1.5 text-2xs leading-relaxed">
              <span
                role="img"
                aria-label={call.ok ? t('trail.succeeded') : t('trail.failed')}
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

// Tone only — the status COPY lives in chat:outcome.* / chat:itemOutcome.*,
// keyed by the same status strings the backend sends.
const OUTCOME_DOT: Record<string, string> = {
  cancelled: 'bg-text-disabled',
  applied: 'bg-semantic-success/70',
  'applied-unresolved': 'bg-amber-400/80',
  declined: 'bg-text-tertiary/60',
  timeout: 'bg-amber-400/80',
  error: 'bg-red-400/80',
  applying: 'bg-brand-400',
  pending: 'bg-brand-400',
}

const ITEM_OUTCOME_DOT: Record<string, string> = {
  applied: 'bg-green-500/70',
  'applied-unresolved': 'bg-amber-400/80',
  error: 'bg-red-400/80',
  'already-resolved': 'bg-text-tertiary/60',
}

// The kind a record without one was: every proposal before previewKind was
// a fix patch (the same default as the live ApprovalCard).
const DEFAULT_PREVIEW_KIND = 'fix-patch'

// A proposal of a kind this bundle does not know, in the generic strip's
// terms: the item keys are a fix's ruleId and block label, as on the live
// card; a single proposal with no item list is its own one item.
function asApproval(snapshot: FixProposalSnapshot, previewKind: string): ApprovalSnapshot {
  const items = snapshot.items ?? []
  return {
    proposalId: snapshot.proposalId,
    tool: snapshot.tool,
    previewKind,
    status: snapshot.status,
    message: snapshot.message,
    items:
      items.length > 0
        ? items.map(it => ({
            id: it.ruleId,
            label: it.blockLabel,
            summary: it.summary,
            status: it.status,
            message: it.message,
          }))
        : [{id: snapshot.ruleId, label: snapshot.blockLabel, summary: snapshot.summary, status: snapshot.status}],
  }
}

// FixOutcomeStrip is the persisted record of an apply_fix / apply_fixes
// approval: what was proposed and how it resolved, attached to the assistant
// message so it survives the stream that produced it. Batches render one
// per-fix row list under the overall status. A proposal a later server
// previewed as another kind has no fix shape to show and takes the generic
// strip instead — the tool's name and its items, never a blank "Fix".
export function FixOutcomeStrip({snapshot}: {snapshot: FixProposalSnapshot}) {
  const {t} = useTranslation('chat')
  const kind = snapshot.previewKind || DEFAULT_PREVIEW_KIND
  if (kind !== DEFAULT_PREVIEW_KIND) return <ApprovalOutcomeStrip snapshot={asApproval(snapshot, kind)} />
  // Unknown status falls back to the raw wire value rather than a blank row.
  const outcome = {
    text: t(`outcome.${snapshot.status}`, {defaultValue: snapshot.status}),
    dot: OUTCOME_DOT[snapshot.status] ?? 'bg-text-tertiary/60',
  }
  const items = snapshot.items ?? []
  const batch = items.length > 1
  return (
    <div
      className="mx-1 rounded-md border border-border-subtle bg-surface-2 px-2 py-1 text-2xs"
      data-testid="fix-outcome"
      data-preview-kind={kind}
    >
      <div className="flex items-baseline gap-1.5">
        <span className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${outcome.dot}`} aria-hidden="true" />
        <span className="font-medium text-text-secondary">
          {batch ? (
            `${items.length} fixes`
          ) : (
            <>
              Fix <code className="font-mono text-brand-300">{snapshot.fixType}</code>
            </>
          )}
          {!batch && snapshot.blockLabel && <span className="text-text-tertiary"> · {snapshot.blockLabel}</span>}
        </span>
        <span className="text-text-tertiary">{outcome.text}</span>
        {snapshot.message && <span className="min-w-0 truncate text-text-tertiary/70">— {snapshot.message}</span>}
      </div>
      {batch && (
        <ul className="mt-1 space-y-0.5">
          {items.map((it, i) => {
            const io = {
              text: t(`itemOutcome.${it.status}`, {defaultValue: it.status}),
              dot: ITEM_OUTCOME_DOT[it.status] ?? 'bg-text-tertiary/60',
            }
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

// ApprovalOutcomeStrip is the persisted record of an approval of any other
// kind — a tool this bundle does not know — worded in the tool's terms like
// the live card's generic preview: the tool, the overall outcome, and the
// items by label (one inline, a batch as rows with their own outcomes).
// Nothing is interpreted; the labels and messages are the server's text.
export function ApprovalOutcomeStrip({snapshot}: {snapshot: ApprovalSnapshot}) {
  const {t} = useTranslation('chat')
  const items = snapshot.items
  const batch = items.length > 1
  // The live card shows a countdown instead of pending copy, so that one
  // status borrows the fix wording; the rest fall back to the raw value.
  const statusKey = snapshot.status === 'pending' ? 'outcome.pending' : `approvalCard.status.${snapshot.status}`
  const outcome = {
    text: t(statusKey, {defaultValue: snapshot.status}),
    dot: OUTCOME_DOT[snapshot.status] ?? 'bg-text-tertiary/60',
  }
  return (
    <div
      className="mx-1 rounded-md border border-border-subtle bg-surface-2 px-2 py-1 text-2xs"
      data-testid="approval-outcome"
      data-preview-kind={snapshot.previewKind || DEFAULT_PREVIEW_KIND}
    >
      <div className="flex items-baseline gap-1.5">
        <span className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${outcome.dot}`} aria-hidden="true" />
        <span className="font-medium text-text-secondary">
          {snapshot.tool ? (
            <code className="font-mono text-brand-300">{snapshot.tool}</code>
          ) : (
            t('approvalCard.heading')
          )}
          {batch ? (
            <span className="text-text-tertiary"> · {t('approvalCard.itemCount', {count: items.length})}</span>
          ) : (
            items[0]?.label && <span className="text-text-tertiary"> · {items[0].label}</span>
          )}
        </span>
        <span className="text-text-tertiary">{outcome.text}</span>
        {snapshot.message && <span className="min-w-0 truncate text-text-tertiary/70">— {snapshot.message}</span>}
      </div>
      {batch && (
        <ul className="mt-1 space-y-0.5">
          {items.map((it, i) => {
            const io = {
              text: t(`approvalCard.itemStatus.${it.status}`, {defaultValue: it.status}),
              dot: ITEM_OUTCOME_DOT[it.status] ?? 'bg-text-tertiary/60',
            }
            return (
              <li key={i} className="flex items-baseline gap-1.5">
                <span className={`mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full ${io.dot}`} aria-hidden="true" />
                <span className="text-text-secondary">
                  {it.label} <span className="text-text-tertiary/60">· {io.text}</span>
                  {it.message && <span className="text-text-tertiary/70"> — {it.message}</span>}
                </span>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
