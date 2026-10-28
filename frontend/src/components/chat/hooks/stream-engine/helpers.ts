import i18n from '@/i18n'
import type {ApprovalCard, ApprovalItem, ChatThread} from '@/stores/chatStore'
import type {ChatMessage, ProviderID} from '@/types'
import type {DoneMeta, FixDecisionEventItem, FixProposalPayload} from '@/lib/chatEvent'
import type {ProviderErrorInfo} from '@/lib/chatErrors'
import type {StreamMeta} from './types'

export function providerErrorLine(info: ProviderErrorInfo, message: string): string {
  return i18n.t('chat:errors.providerBubble', {title: i18n.t(`chat:${info.titleKey}`), message})
}

export function pendingCard(p: FixProposalPayload): ApprovalCard {
  const first = p.items[0]
  return {
    proposalId: p.proposalId,
    tool: p.tool,
    previewKind: p.previewKind,
    source: p.source,
    ...(p.requiredScope ? {requiredScope: p.requiredScope} : {}),
    status: 'pending',
    items: p.items.map(it => ({...it, status: 'pending' as const})),
    ruleId: first?.ruleId ?? '',
    fixType: first?.fixType ?? '',
    blockLabel: first?.blockLabel ?? '',
    line: first?.line ?? 0,
    summary: first?.summary ?? '',
  }
}

export function decisionTargets(
  rows: readonly ApprovalItem[],
  items: readonly FixDecisionEventItem[],
): {index: number; item: FixDecisionEventItem}[] {
  const taken = new Set<number>()
  const out: {index: number; item: FixDecisionEventItem}[] = []
  items.forEach((item, i) => {
    const sameRule = (row: ApprovalItem) => !item.ruleId || !row.ruleId || row.ruleId === item.ruleId
    const index = item.id
      ? rows.findIndex(row => row.id === item.id)
      : i < rows.length && !taken.has(i) && sameRule(rows[i]!)
        ? i
        : rows.findIndex((row, j) => !taken.has(j) && sameRule(row))
    if (index < 0 || taken.has(index)) return
    taken.add(index)
    out.push({index, item})
  })
  return out
}

const CARD_STATUSES: readonly string[] = [
  'applying',
  'applied',
  'applied-unresolved',
  'already-resolved',
  'declined',
  'timeout',
  'error',
  'cancelled',
]
const ITEM_STATUSES: readonly string[] = ['applied', 'applied-unresolved', 'error', 'already-resolved', 'skipped']
export const asCardStatus = (s: string): ApprovalCard['status'] =>
  CARD_STATUSES.includes(s) ? (s as ApprovalCard['status']) : 'error'
export const asItemStatus = (s: string): ApprovalItem['status'] =>
  ITEM_STATUSES.includes(s) ? (s as ApprovalItem['status']) : 'error'

export function applyDoneMeta(msg: ChatMessage, meta: DoneMeta | undefined): void {
  if (!meta) return
  if (meta.servedModel) msg.servedModel = meta.servedModel
}

export function resumedTurn(thread: ChatThread, streamId: string): {meta: StreamMeta; messageId?: string} | null {
  const rec = thread.inFlight
  if (!rec || rec.streamId !== streamId) return null
  return {
    meta: {provider: rec.provider, model: rec.model, ...(rec.modelLabel ? {modelLabel: rec.modelLabel} : {})},
    ...(rec.messageId ? {messageId: rec.messageId} : {}),
  }
}

export function messageModel(acc: Partial<StreamMeta>): Pick<ChatMessage, 'provider' | 'model' | 'modelLabel'> {
  if (!acc.provider) return {}
  return {provider: acc.provider, model: acc.model ?? '', ...(acc.modelLabel ? {modelLabel: acc.modelLabel} : {})}
}

export const ALIAS_PROVIDERS: ReadonlySet<ProviderID> = new Set<ProviderID>(['azure-foundry'])
