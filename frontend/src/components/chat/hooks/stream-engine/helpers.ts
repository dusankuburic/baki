import i18n from '@/i18n'
import type {FixProposalCard, FixProposalItem, ChatThread} from '@/stores/chatStore'
import type {ChatMessage, ProviderID} from '@/types'
import type {DoneMeta, FixProposalPayload} from '@/lib/chatEvent'
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
export function pendingCard(p: FixProposalPayload): FixProposalCard {
  const first = p.items[0]
  return {
    proposalId: p.proposalId,
    status: 'pending',
    items: p.items.map(it => ({...it, status: 'pending' as const})),
    ruleId: first?.ruleId ?? '',
    fixType: first?.fixType ?? '',
    blockLabel: first?.blockLabel ?? '',
    line: first?.line ?? 0,
    summary: first?.summary ?? '',
  }
}

// Only statuses the card and item types know are mapped through; an unknown
// one still clears the card's buttons via 'error'.
const CARD_STATUSES: readonly string[] = ['applying', 'applied', 'applied-unresolved', 'declined', 'timeout', 'error']
const ITEM_STATUSES: readonly string[] = ['applied', 'applied-unresolved', 'error', 'already-resolved']
export const asCardStatus = (s: string): FixProposalCard['status'] =>
  CARD_STATUSES.includes(s) ? (s as FixProposalCard['status']) : 'error'
export const asItemStatus = (s: string): FixProposalItem['status'] =>
  ITEM_STATUSES.includes(s) ? (s as FixProposalItem['status']) : 'error'

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
