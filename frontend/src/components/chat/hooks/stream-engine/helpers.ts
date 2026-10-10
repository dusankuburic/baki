import i18n from '@/i18n'
import type {ApprovalCard, ApprovalItem, ChatThread} from '@/stores/chatStore'
import type {ChatMessage, ProviderID} from '@/types'
import type {DoneMeta, FixDecisionEventItem, FixProposalPayload} from '@/lib/chatEvent'
import type {ProviderErrorInfo} from '@/lib/chatErrors'
import type {StreamMeta} from './types'

// providerErrorLine is the error bubble's text for a provider failure: the
// localized title, then the server's message (which names the deployment,
// role or limit at fault) as the detail.
export function providerErrorLine(info: ProviderErrorInfo, message: string): string {
  return i18n.t('chat:errors.providerBubble', {title: i18n.t(`chat:${info.titleKey}`), message})
}

// pendingCard primes an approval card from its wire payload; the flat fields
// mirror items[0] for single-fix prompts. Used by the live event and the
// resume journal replay alike.
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

// decisionTargets pairs a decision's per-item outcomes with the card's rows:
// by the id the server stamped on both (blockId:ruleId, so in any order),
// else — an older server, which sends the rule alone — by position, checked
// against the rule. Never the first row with the same rule: a batch that
// fixed one rule on two blocks showed block A's outcome on block B.
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

// Only statuses the card and item types know are mapped through; an unknown
// one still clears the card's buttons via 'error'.
const CARD_STATUSES: readonly string[] = [
  'applying',
  'applied',
  'applied-unresolved',
  'declined',
  'timeout',
  'error',
  'cancelled',
]
const ITEM_STATUSES: readonly string[] = ['applied', 'applied-unresolved', 'error', 'already-resolved']
export const asCardStatus = (s: string): ApprovalCard['status'] =>
  CARD_STATUSES.includes(s) ? (s as ApprovalCard['status']) : 'error'
export const asItemStatus = (s: string): ApprovalItem['status'] =>
  ITEM_STATUSES.includes(s) ? (s as ApprovalItem['status']) : 'error'

// applyDoneMeta pins a done's served model and billed rates onto a message —
// the done commit and the billed-error bubble share the fields.
export function applyDoneMeta(msg: ChatMessage, meta: DoneMeta | undefined): void {
  if (!meta) return
  if (meta.servedModel) msg.servedModel = meta.servedModel
  if (meta.inputCostPerM !== undefined) msg.inputCostPerM = meta.inputCostPerM
  if (meta.outputCostPerM !== undefined) msg.outputCostPerM = meta.outputCostPerM
}

// resumedTurn is what a re-attached stream is labelled with: the thread's
// record of the turn in flight (chatStore.InFlightTurn) — the model the turn
// was actually sent to, a stand-in's included — and the message id the
// server stores its answer under. The payload of a conversation load does
// not say which model the in-flight turn used, so without a record nothing
// is guessed (see resumeAfterReload).
export function resumedTurn(thread: ChatThread, streamId: string): {meta: StreamMeta; messageId?: string} | null {
  const rec = thread.inFlight
  if (!rec || rec.streamId !== streamId) return null
  return {
    meta: {provider: rec.provider, model: rec.model, ...(rec.modelLabel ? {modelLabel: rec.modelLabel} : {})},
    ...(rec.messageId ? {messageId: rec.messageId} : {}),
  }
}

// The model fields a committed message (or error bubble) carries: none for
// a stream with no known provider (a re-attached one with no record).
export function messageModel(acc: Partial<StreamMeta>): Pick<ChatMessage, 'provider' | 'model' | 'modelLabel'> {
  if (!acc.provider) return {}
  return {provider: acc.provider, model: acc.model ?? '', ...(acc.modelLabel ? {modelLabel: acc.modelLabel} : {})}
}

// ALIAS_PROVIDERS resolve the selected model to another one at answer time
// (Azure AI Foundry: a deployment, `model-router`, an agent). Their model
// listing names the served model once one is known, so the first done that
// reports one refetches /api/providers/list — once per
// thread+deployment+served-model triple, not per turn.
export const ALIAS_PROVIDERS: ReadonlySet<ProviderID> = new Set<ProviderID>(['azure-foundry'])
