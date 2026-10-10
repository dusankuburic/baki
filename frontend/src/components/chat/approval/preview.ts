import type {ApprovalCard as ApprovalCardState} from '@/stores/chatStore'

// What a preview renders with: the request, and the per-item opt-out state
// the frame owns (the frame sends the decision; a preview only shows the
// request and lets a batch be trimmed).
export interface ApprovalPreviewProps {
  approval: ApprovalCardState
  pending: boolean
  // More than one item: rows with checkboxes, a per-item outcome each.
  batch: boolean
  // Indices in approval.items the user deselected (U4.1).
  excluded: ReadonlySet<number>
  includedCount: number
  onToggle: (index: number) => void
}

// Status copy lives in chat:fixCard.* / chat:approvalCard.*, keyed by the
// same status strings the backend sends. Only tone stays here.
export const ITEM_STATUS_TONE: Record<string, string> = {
  applied: 'text-semantic-success',
  'applied-unresolved': 'text-semantic-warning',
  error: 'text-semantic-error',
  'already-resolved': 'text-text-tertiary',
}
