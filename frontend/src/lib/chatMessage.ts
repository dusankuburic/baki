import type {
  ApprovalItemSnapshot,
  ApprovalSnapshot,
  ChatMessage,
  FixItemSnapshot,
  FixProposalSnapshot,
  ToolCallRecord,
} from '@/types'
import {storableModelLabel} from './modelIds'
import {asKnowledgeStatus} from './chatEvent'

const KNOWN_ROLES = new Set(['user', 'assistant', 'system'])

function parseToolCalls(raw: unknown): ToolCallRecord[] | undefined {
  if (!Array.isArray(raw)) return undefined
  const out: ToolCallRecord[] = []
  for (const entry of raw) {
    if (!entry || typeof entry !== 'object') continue
    const c = entry as Record<string, unknown>
    if (typeof c.name !== 'string' || !c.name) continue
    const rec: ToolCallRecord = {name: c.name, ok: c.ok === true}
    if (typeof c.label === 'string' && c.label) rec.label = c.label
    if (typeof c.durationMs === 'number') rec.durationMs = c.durationMs
    if (typeof c.summary === 'string' && c.summary) rec.summary = c.summary
    out.push(rec)
  }
  return out.length > 0 ? out : undefined
}

function parseFixItem(raw: unknown): FixItemSnapshot | null {
  if (!raw || typeof raw !== 'object') return null
  const i = raw as Record<string, unknown>
  if (typeof i.ruleId !== 'string' || !i.ruleId) return null
  const item: FixItemSnapshot = {
    ruleId: i.ruleId,
    fixType: typeof i.fixType === 'string' ? i.fixType : '',
    blockLabel: typeof i.blockLabel === 'string' ? i.blockLabel : '',
    line: typeof i.line === 'number' ? i.line : 0,
    summary: typeof i.summary === 'string' ? i.summary : '',
    status: typeof i.status === 'string' ? i.status : '',
  }
  if (typeof i.message === 'string' && i.message) item.message = i.message
  return item
}

function parseFixProposal(raw: unknown): FixProposalSnapshot | null {
  if (!raw || typeof raw !== 'object') return null
  const p = raw as Record<string, unknown>
  if (typeof p.proposalId !== 'string' || !p.proposalId) return null
  const snap: FixProposalSnapshot = {
    proposalId: p.proposalId,
    ruleId: typeof p.ruleId === 'string' ? p.ruleId : '',
    fixType: typeof p.fixType === 'string' ? p.fixType : '',
    blockLabel: typeof p.blockLabel === 'string' ? p.blockLabel : '',
    line: typeof p.line === 'number' ? p.line : 0,
    summary: typeof p.summary === 'string' ? p.summary : '',
    status: typeof p.status === 'string' ? p.status : '',
  }
  if (typeof p.tool === 'string' && p.tool) snap.tool = p.tool
  if (typeof p.previewKind === 'string' && p.previewKind) snap.previewKind = p.previewKind
  if (typeof p.message === 'string' && p.message) snap.message = p.message
  if (Array.isArray(p.items)) {
    const items: FixItemSnapshot[] = []
    for (const entry of p.items) {
      const item = parseFixItem(entry)
      if (item) items.push(item)
    }
    if (items.length > 0) snap.items = items
  }
  return snap
}

function parseApprovalItem(raw: unknown): ApprovalItemSnapshot | null {
  if (!raw || typeof raw !== 'object') return null
  const i = raw as Record<string, unknown>
  const item: ApprovalItemSnapshot = {
    id: typeof i.id === 'string' ? i.id : '',
    label: typeof i.label === 'string' ? i.label : '',
    summary: typeof i.summary === 'string' ? i.summary : '',
    status: typeof i.status === 'string' ? i.status : '',
  }
  if (typeof i.message === 'string' && i.message) item.message = i.message
  return item
}

function parseApproval(raw: unknown): ApprovalSnapshot | null {
  if (!raw || typeof raw !== 'object') return null
  const a = raw as Record<string, unknown>
  if (typeof a.proposalId !== 'string' || !a.proposalId) return null
  const items: ApprovalItemSnapshot[] = []
  if (Array.isArray(a.items)) {
    for (const entry of a.items) {
      const item = parseApprovalItem(entry)
      if (item) items.push(item)
    }
  }
  const snap: ApprovalSnapshot = {
    proposalId: a.proposalId,
    status: typeof a.status === 'string' ? a.status : '',
    items,
  }
  if (typeof a.tool === 'string' && a.tool) snap.tool = a.tool
  if (typeof a.previewKind === 'string' && a.previewKind) snap.previewKind = a.previewKind
  if (typeof a.requiredScope === 'string' && a.requiredScope) snap.requiredScope = a.requiredScope
  if (typeof a.message === 'string' && a.message) snap.message = a.message
  return snap
}

export function parseChatMessage(raw: unknown): ChatMessage | null {
  if (!raw || typeof raw !== 'object') return null
  const m = raw as Record<string, unknown>

  const id = typeof m.id === 'string' ? m.id : ''
  const timestamp = typeof m.timestamp === 'string' ? m.timestamp : ''
  if (!id || !timestamp) return null

  const role = typeof m.role === 'string' && KNOWN_ROLES.has(m.role) ? (m.role as ChatMessage['role']) : 'assistant'

  const out: ChatMessage = {
    id,
    role,
    content: typeof m.content === 'string' ? m.content : '',
    timestamp,
  }
  if (typeof m.contextBlockId === 'string' && m.contextBlockId) out.contextBlockId = m.contextBlockId
  if (typeof m.tokensIn === 'number') out.tokensIn = m.tokensIn
  if (typeof m.tokensOut === 'number') out.tokensOut = m.tokensOut
  if (typeof m.provider === 'string' && m.provider) out.provider = m.provider as ChatMessage['provider']
  if (typeof m.model === 'string' && m.model) out.model = m.model
  const modelLabel = typeof m.modelLabel === 'string' ? storableModelLabel(m.modelLabel, out.model) : undefined
  if (modelLabel) out.modelLabel = modelLabel
  if (typeof m.servedModel === 'string' && m.servedModel) out.servedModel = m.servedModel
  if (typeof m.finishReason === 'string') out.finishReason = m.finishReason as ChatMessage['finishReason']
  // What the turn recorded about itself, so a reloaded message shows what it showed live.
  if (typeof m.errorCode === 'string' && m.errorCode) out.errorCode = m.errorCode
  if (m.errorShared === true) out.errorShared = true
  if (typeof m.truncated === 'number' && Number.isInteger(m.truncated) && m.truncated > 0) out.truncated = m.truncated
  const knowledge = asKnowledgeStatus(m.knowledge)
  if (knowledge) out.knowledge = knowledge

  const toolCalls = parseToolCalls(m.toolCalls)
  if (toolCalls) out.toolCalls = toolCalls
  const single = parseFixProposal(m.fixProposal)
  if (single) out.fixProposal = single
  if (Array.isArray(m.fixProposals)) {
    const proposals: FixProposalSnapshot[] = []
    for (const entry of m.fixProposals) {
      const parsed = parseFixProposal(entry)
      if (parsed) proposals.push(parsed)
    }
    if (proposals.length > 0) out.fixProposals = proposals
  }
  if (Array.isArray(m.approvals)) {
    const approvals: ApprovalSnapshot[] = []
    for (const entry of m.approvals) {
      const parsed = parseApproval(entry)
      if (parsed) approvals.push(parsed)
    }
    if (approvals.length > 0) out.approvals = approvals
  }
  return out
}

export function parseChatMessages(raw: unknown): ChatMessage[] {
  if (!Array.isArray(raw)) return []
  const out: ChatMessage[] = []
  for (const m of raw) {
    const parsed = parseChatMessage(m)
    if (parsed) out.push(parsed)
  }
  return out
}

export function parseConversationPayload(raw: unknown): ChatMessage[] {
  if (raw == null) return []
  if (Array.isArray(raw)) return parseChatMessages(raw)
  if (typeof raw === 'object' && Array.isArray((raw as {messages?: unknown}).messages)) {
    return parseChatMessages((raw as {messages: unknown[]}).messages)
  }
  throw new Error('Unexpected conversation payload')
}
