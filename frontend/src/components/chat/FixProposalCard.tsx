import {FIX_DECISION_WINDOW_S} from '@/lib/constants'
import {useEffect, useRef, useState} from 'react'
import type {FixProposalCard as FixProposalCardState, FixProposalItem} from '@/stores/chatStore'
import {PatchPreviewText} from '@/components/shared'

interface FixProposalCardProps {
  proposal: FixProposalCardState
  // excludedItemIndices carries per-item OPT-OUTS for batch approvals (U4.1):
  // positions in proposal.items the user deselected before approving.
  onRespond: (approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => void
}

// Mirrors the backend's FixDecisionTimeout (internal/ai/tools.go): the server
// is authoritative (its fix_decision event resolves the card), this only makes
// the window visible so the timeout isn't a surprise.
const DECISION_WINDOW_S = FIX_DECISION_WINDOW_S

// Status line copy for resolved proposals (pending/applying render inline).
const STATUS_TEXT: Record<string, string> = {
  applying: 'Applying the fix…',
  applied: 'Fix applied and verified.',
  'applied-unresolved': 'Applied, but the finding still appears — review recommended.',
  declined: 'Declined — nothing was changed.',
  timeout: 'No response in time — nothing was changed.',
  error: 'Fix failed.',
}

const ITEM_STATUS_TEXT: Record<string, string> = {
  applied: 'applied',
  'applied-unresolved': 'applied — still appears',
  error: 'failed',
  'already-resolved': 'already resolved',
}

const ITEM_STATUS_TONE: Record<string, string> = {
  applied: 'text-semantic-success',
  'applied-unresolved': 'text-semantic-warning',
  error: 'text-semantic-error',
  'already-resolved': 'text-text-tertiary',
}

function itemStatusLine(item: FixProposalItem): string {
  return ITEM_STATUS_TEXT[item.status] ?? item.status
}

/**
 * Approval prompt for AI-proposed source fixes (apply_fix / apply_fixes).
 * Nothing is written to the flow unless the user clicks Approve; summaries
 * come from the server-rendered patch previews (already secret-scrubbed).
 * A batch renders one card with a per-fix row list; per-item outcomes land
 * on the rows when the batch decision resolves.
 */
export default function FixProposalCard({proposal, onRespond}: FixProposalCardProps) {
  const pending = proposal.status === 'pending'
  const statusText = STATUS_TEXT[proposal.status]
  const batch = proposal.items.length > 1
  const cardRef = useRef<HTMLDivElement>(null)
  const [remaining, setRemaining] = useState(DECISION_WINDOW_S)
  // Deselected batch items (U4.1): default = everything checked. Only
  // meaningful while pending; frozen cards ignore it.
  const [excluded, setExcluded] = useState<Set<number>>(new Set())
  const includedCount = proposal.items.length - [...excluded].filter(i => i < proposal.items.length).length

  // Countdown while awaiting the decision: the server-side window is real
  // (expiry declines the proposal server-side), but was invisible until the
  // card flipped to "No response in time".
  useEffect(() => {
    if (!pending) return
    const startedAt = Date.now()
    const t = setInterval(() => {
      const left = DECISION_WINDOW_S - Math.floor((Date.now() - startedAt) / 1000)
      setRemaining(left > 0 ? left : 0)
    }, 1000)
    return () => clearInterval(t)
  }, [pending])

  // alertdialog contract: move focus to the dialog so screen readers announce
  // it and keyboard users land at the decision (the container, NOT Approve —
  // focusing a button invites accidental Enter-approval). Escape dismisses.
  useEffect(() => {
    if (pending) cardRef.current?.focus()
  }, [pending])

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (pending && e.key === 'Escape') {
      e.stopPropagation()
      onRespond(false, proposal.proposalId)
    }
  }

  return (
    <div
      ref={cardRef}
      tabIndex={-1}
      onKeyDown={onKeyDown}
      className="mx-3 my-2 rounded-lg border border-brand-500/40 bg-brand-500/5 p-3 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-brand-500/50"
      data-testid="fix-proposal-card"
      role="alertdialog"
      aria-label={batch ? `Proposed ${proposal.items.length} fixes: approval needed` : `Proposed fix: ${proposal.fixType}`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-2xs font-semibold uppercase tracking-wide text-brand-300">
          {batch ? `Proposed ${proposal.items.length} fixes — approval needed` : 'Proposed fix — approval needed'}
        </span>
        {batch ? (
          <span className="text-2xs text-text-tertiary">
            {pending && excluded.size > 0 ? `applying ${includedCount} of ${proposal.items.length}` : 'select the fixes to apply'}
          </span>
        ) : (
          <code className="rounded bg-surface-3 px-1.5 py-0.5 font-mono text-2xs text-brand-300">{proposal.fixType}</code>
        )}
      </div>

      {batch ? (
        <ul className="mt-2 space-y-1.5" data-testid="fix-proposal-items">
          {proposal.items.map((item, i) => (
            <li key={i} className="rounded border border-border-subtle bg-surface-2/60 p-1.5">
              <div className="flex items-baseline justify-between gap-2">
                <span className="flex min-w-0 items-baseline gap-1.5">
                  {pending && (
                    <input
                      type="checkbox"
                      checked={!excluded.has(i)}
                      onChange={() =>
                        setExcluded(prev => {
                          const next = new Set(prev)
                          if (next.has(i)) next.delete(i)
                          else next.add(i)
                          return next
                        })
                      }
                      className="mt-0.5 shrink-0 accent-brand-500"
                      aria-label={`Include fix for ${item.blockLabel}`}
                      data-testid={`include-item-${i}`}
                    />
                  )}
                  <span className="truncate text-xs text-text-secondary">
                    {item.blockLabel}
                    {item.line > 0 && <span className="text-text-tertiary"> · line {item.line}</span>}
                  </span>
                </span>
                <code className="shrink-0 rounded bg-surface-3 px-1.5 py-0.5 font-mono text-2xs text-brand-300">{item.fixType}</code>
              </div>
              {item.summary && <PatchPreviewText text={item.summary} className="mt-1 max-h-24" />}
              {item.status !== 'pending' && (
                <p className={'mt-1 text-2xs ' + (ITEM_STATUS_TONE[item.status] ?? 'text-text-tertiary')} data-testid="fix-item-status">
                  {itemStatusLine(item)}
                  {item.message ? ` — ${item.message}` : ''}
                </p>
              )}
            </li>
          ))}
        </ul>
      ) : (
        <>
          <div className="mt-1 text-xs text-text-secondary">
            {proposal.blockLabel}
            {proposal.line > 0 && <span className="text-text-tertiary"> · line {proposal.line}</span>}
            <span className="text-text-tertiary"> · rule </span>
            <code className="font-mono text-2xs text-text-secondary">{proposal.ruleId}</code>
          </div>
          {proposal.summary && <PatchPreviewText text={proposal.summary} className="mt-2 max-h-40" />}
        </>
      )}

      {pending ? (
        <div className="mt-2 flex items-center gap-2">
          <button
            type="button"
            className="rounded bg-brand-600 px-3 py-1 text-2xs font-medium text-brand-foreground transition-colors hover:bg-brand-700 disabled:opacity-50"
            onClick={() => onRespond(true, proposal.proposalId, batch ? [...excluded] : undefined)}
            disabled={batch && includedCount === 0}
          >
            {batch ? `Approve & apply ${includedCount} ${includedCount !== 1 ? 'fixes' : 'fix'}` : 'Approve & apply'}
          </button>
          <button
            type="button"
            className="rounded border border-border-default bg-surface-3 px-3 py-1 text-2xs font-medium text-text-secondary transition-colors hover:bg-surface-4 hover:text-text-primary disabled:opacity-50"
            onClick={() => onRespond(false, proposal.proposalId)}
          >
            Dismiss
          </button>
          <span
            className={remaining <= 10 ? 'text-2xs text-semantic-warning' : 'text-2xs text-text-tertiary/70'}
            aria-live="off"
            title="The proposal is declined automatically when this window expires"
          >
            {remaining > 0 ? `${remaining}s to decide` : 'expiring…'}
          </span>
        </div>
      ) : (
        statusText && (
          <p
            className={
              'mt-2 text-2xs ' +
              (proposal.status === 'applied'
                ? 'text-semantic-success'
                : proposal.status === 'error'
                  ? 'text-semantic-error'
                  : 'text-text-tertiary')
            }
            data-testid="fix-proposal-status"
          >
            {statusText}
            {proposal.message && proposal.status === 'error' ? ` ${proposal.message}` : ''}
          </p>
        )
      )}
    </div>
  )
}
