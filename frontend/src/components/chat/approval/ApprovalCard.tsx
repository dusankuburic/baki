import {useTranslation} from 'react-i18next'
import {useEffect, useRef, useState} from 'react'
import type {ComponentType} from 'react'
import {FIX_DECISION_WINDOW_S} from '@/lib/constants'
import type {ApprovalCard as ApprovalCardState} from '@/stores/chatStore'
import type {ApprovalPreviewProps} from './preview'
import FixPatchPreview from './previews/FixPatchPreview'
import GenericPreview from './previews/GenericPreview'

interface ApprovalCardProps {
  approval: ApprovalCardState
  // excludedItemIndices carries per-item OPT-OUTS for batch approvals (U4.1):
  // positions in approval.items the user deselected before approving.
  onRespond: (approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => void
}

// Mirrors the backend's FixDecisionTimeout (internal/ai/tools.go): the server
// is authoritative (its decision event resolves the card), this only makes
// the window visible so the timeout isn't a surprise.
const DECISION_WINDOW_S = FIX_DECISION_WINDOW_S

function secondsLeft(startedAt: number): number {
  return Math.max(0, DECISION_WINDOW_S - Math.floor((Date.now() - startedAt) / 1000))
}

// The previews by the server's previewKind. A kind this bundle does not know
// renders the generic preview (the request is still decided, in the tool's
// own terms); a server that sends no kind sent a fix.
const DEFAULT_PREVIEW_KIND = 'fix-patch'
const PREVIEWS: Record<string, ComponentType<ApprovalPreviewProps>> = {
  'fix-patch': FixPatchPreview,
}

function previewKindOf(approval: ApprovalCardState): string {
  return approval.previewKind || DEFAULT_PREVIEW_KIND
}

/**
 * Approval prompt for a tool call the server will not run without the user's
 * say-so (apply_fix / apply_fixes today). Nothing is written unless the
 * user clicks Approve; the preview is the server's rendering of the request
 * (secret-scrubbed), chosen by its previewKind. A batch renders one card
 * with a per-item row list; per-item outcomes land on the rows when the
 * batch decision resolves.
 */
export default function ApprovalCard({approval, onRespond}: ApprovalCardProps) {
  const {t} = useTranslation('chat')
  const kind = previewKindOf(approval)
  // An own key only: the kind is the server's string, and a prototype
  // member's name (`constructor`) must not resolve to Object.prototype.
  const Preview = Object.prototype.hasOwnProperty.call(PREVIEWS, kind) ? PREVIEWS[kind]! : GenericPreview
  // The fix card keeps its own copy (chat:fixCard.*); any other kind is
  // worded in the tool's terms (chat:approvalCard.*).
  const copy = kind === DEFAULT_PREVIEW_KIND ? 'fixCard' : 'approvalCard'
  const pending = approval.status === 'pending'
  const statusText = t(`${copy}.status.${approval.status}`, {defaultValue: ''})
  const batch = approval.items.length > 1
  const cardRef = useRef<HTMLDivElement>(null)
  // The countdown runs from when the request reached this client
  // (receivedAt, stamped by the store), not from when this card mounted: the
  // card remounts on thread switch-back and pop-out while the server's window
  // keeps running, and a mount-based countdown restarted at the full minute.
  // Mount time is only the fallback for a card that carries no stamp.
  const [startedAt] = useState(() => approval.receivedAt ?? Date.now())
  const [remaining, setRemaining] = useState(() => secondsLeft(startedAt))
  // Deselected batch items (U4.1): default = everything checked. Only
  // meaningful while pending; frozen cards ignore it.
  const [excluded, setExcluded] = useState<Set<number>>(new Set())
  const includedCount = approval.items.length - [...excluded].filter(i => i < approval.items.length).length

  // Countdown while awaiting the decision: the server-side window is real
  // (expiry declines the request server-side), but was invisible until the
  // card flipped to "No response in time".
  useEffect(() => {
    if (!pending) return
    const timer = setInterval(() => setRemaining(secondsLeft(startedAt)), 1000)
    return () => clearInterval(timer)
  }, [pending, startedAt])

  // alertdialog contract: move focus to the dialog so screen readers announce
  // it and keyboard users land at the decision (the container, NOT Approve —
  // focusing a button invites accidental Enter-approval). Escape dismisses.
  // Not while the user is typing, though: stealing focus from the composer
  // dropped their keystrokes, and a reflex Escape then declined the request.
  useEffect(() => {
    if (!pending) return
    const active = document.activeElement
    const typing =
      active instanceof HTMLTextAreaElement ||
      active instanceof HTMLInputElement ||
      (active instanceof HTMLElement && active.isContentEditable)
    if (!typing) cardRef.current?.focus()
  }, [pending])

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (pending && e.key === 'Escape') {
      e.stopPropagation()
      onRespond(false, approval.proposalId)
    }
  }

  const onToggle = (i: number) =>
    setExcluded(prev => {
      const next = new Set(prev)
      if (next.has(i)) next.delete(i)
      else next.add(i)
      return next
    })

  const isFix = copy === 'fixCard'
  const ariaLabel = isFix
    ? batch
      ? t('fixCard.ariaBatch', {count: approval.items.length})
      : t('fixCard.aria', {fixType: approval.fixType})
    : t('approvalCard.aria', {tool: approval.tool})
  const announce = isFix
    ? batch
      ? t('fixCard.announceBatch', {count: approval.items.length})
      : t('fixCard.announce')
    : t('approvalCard.announce', {tool: approval.tool})
  const approveLabel = batch ? t(`${copy}.approveBatch`, {count: includedCount}) : t(`${copy}.approve`)
  // The server's message is the explanation of a failure or a cancellation;
  // the other outcomes say it all themselves.
  const showMessage = !!approval.message && (approval.status === 'error' || approval.status === 'cancelled')

  return (
    <div
      ref={cardRef}
      tabIndex={-1}
      onKeyDown={onKeyDown}
      className="my-2 rounded-lg border border-brand-500/40 bg-brand-500/5 p-3 text-left focus:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/50"
      data-testid="fix-proposal-card"
      data-preview-kind={kind}
      role="alertdialog"
      aria-label={ariaLabel}
    >
      {/* Announced without moving focus: the card no longer takes focus while
          the user is typing, and a screen-reader user still needs to hear that
          a request awaits their decision before the window runs out. */}
      {pending && (
        <span className="sr-only" role="status">
          {announce}
        </span>
      )}

      <Preview
        approval={approval}
        pending={pending}
        batch={batch}
        excluded={excluded}
        includedCount={includedCount}
        onToggle={onToggle}
      />

      {pending ? (
        <div className="mt-2 flex items-center gap-2">
          <button
            type="button"
            className="rounded bg-brand-600 px-3 py-1 text-2xs font-medium text-brand-foreground transition-colors hover:bg-brand-700 disabled:opacity-50"
            onClick={() => onRespond(true, approval.proposalId, batch ? [...excluded] : undefined)}
            disabled={batch && includedCount === 0}
          >
            {approveLabel}
          </button>
          <button
            type="button"
            className="rounded border border-border-default bg-surface-3 px-3 py-1 text-2xs font-medium text-text-secondary transition-colors hover:bg-surface-4 hover:text-text-primary disabled:opacity-50"
            onClick={() => onRespond(false, approval.proposalId)}
          >
            {t(`${copy}.dismiss`)}
          </button>
          <span
            className={remaining <= 10 ? 'text-2xs text-semantic-warning' : 'text-2xs text-text-tertiary/70'}
            aria-live="off"
            title={t('fixCard.expiryTitle')}
          >
            {remaining > 0 ? t('fixCard.secondsToDecide', {count: remaining}) : t('fixCard.expiring')}
          </span>
        </div>
      ) : (
        statusText && (
          <p
            className={
              'mt-2 text-2xs ' +
              (approval.status === 'applied'
                ? 'text-semantic-success'
                : approval.status === 'error'
                  ? 'text-semantic-error'
                  : 'text-text-tertiary')
            }
            data-testid="fix-proposal-status"
          >
            {statusText}
            {showMessage ? ` ${approval.message}` : ''}
          </p>
        )
      )}
    </div>
  )
}
