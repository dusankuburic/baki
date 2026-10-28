import type {ApprovalCard as ApprovalCardState} from '@/stores/chatStore'

export interface ApprovalPreviewProps {
  approval: ApprovalCardState
  pending: boolean
  batch: boolean
  excluded: ReadonlySet<number>
  includedCount: number
  onToggle: (index: number) => void
}

export const ITEM_STATUS_TONE: Record<string, string> = {
  applied: 'text-semantic-success',
  'applied-unresolved': 'text-semantic-warning',
  error: 'text-semantic-error',
  'already-resolved': 'text-text-tertiary',
  skipped: 'text-text-tertiary',
}
