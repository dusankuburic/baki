
export interface DoneMeta {
  servedModel?: string
  /**
   * Where the server's own writes left the stored conversation (ADR 0031, addendum): the version it holds after
   * this turn's question and answer, and the version the turn began from. A client still on the base version may
   * move to the new one without a round trip; present only when the server stored the turn and nobody else wrote
   * to the thread meanwhile.
   */
  conversationVersion?: number
  conversationBase?: number
  /** False when the server could not store this server-owned turn's answer: the client saves the turn itself. */
  answerStored?: false
}

export interface ErrorUsage {
  tokensIn?: number
  tokensOut?: number
  shared?: boolean
  meta?: DoneMeta
}

export type ChatEvent =
  | {kind: 'chunk'; content: string}
  | ({kind: 'done'; tokensOut: number; tokensIn: number; chunks?: number} & DoneMeta)
  | ({kind: 'error'; message: string; code?: string} & ErrorUsage)
  | {kind: 'heartbeat'}
  | {kind: 'tool'; label: string}
  | NoticePayload
  | {kind: 'tool-result'; name: string; label: string; ok: boolean; durationMs: number; summary: string}
  | {
      kind: 'fix-proposal'
      source: ApprovalSource
      proposalId: string
      tool: string
      previewKind: string
      requiredScope?: string
      items: FixProposalEventItem[]
    }
  | {
      kind: 'fix-decision'
      source: ApprovalSource
      proposalId: string
      status: string
      message?: string
      items?: FixDecisionEventItem[]
    }

export type ApprovalSource = 'fix' | 'approval'

/**
 * A journaled notice about the turn itself. history_truncated: the server left `dropped` earlier history
 * messages out to fit the model's context window, `keptFrom` of them in all so far. knowledge: the org's knowledge
 * base was not consulted for this turn, `status` says why (a status this client does not know is dropped).
 */
export interface NoticePayload {
  kind: 'notice'
  notice: string
  dropped?: number
  keptFrom?: number
  status?: KnowledgeStatus
}

export const NOTICE_HISTORY_TRUNCATED = 'history_truncated'
export const NOTICE_KNOWLEDGE = 'knowledge'

/**
 * Why a turn's answer went without the org's knowledge base (R-knowledge-1): the member's embedding model is not
 * the one the corpus was indexed with, the member has no embedding provider, or the lookup ran out of time.
 */
export type KnowledgeStatus = 'identity_mismatch' | 'no_embedder' | 'deadline'

const KNOWLEDGE_STATUSES: ReadonlySet<string> = new Set<KnowledgeStatus>([
  'identity_mismatch',
  'no_embedder',
  'deadline',
])

export function asKnowledgeStatus(value: unknown): KnowledgeStatus | null {
  return typeof value === 'string' && KNOWLEDGE_STATUSES.has(value) ? (value as KnowledgeStatus) : null
}

export interface FixProposalEventItem {
  id: string
  label: string
  /** The block the fix targets, when the item names one (fix-patch items). */
  blockId?: string
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
}

export interface FixDecisionEventItem {
  id?: string
  ruleId?: string
  status: string
  message?: string
}

export type FixProposalPayload = Extract<ChatEvent, {kind: 'fix-proposal'}>

export type ToolResultPayload = Extract<ChatEvent, {kind: 'tool-result'}>

function asRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : null
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null
}

function asNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function asVersion(value: unknown): number | null {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null
}

function doneMeta(data: Record<string, unknown>): DoneMeta {
  const meta: DoneMeta = {}
  const servedModel = asString(data.servedModel)
  if (servedModel) meta.servedModel = servedModel
  const conversationVersion = asVersion(data.conversationVersion)
  const conversationBase = asVersion(data.conversationBase)
  if (conversationVersion !== null && conversationBase !== null) {
    meta.conversationVersion = conversationVersion
    meta.conversationBase = conversationBase
  }
  if (data.answerStored === false) meta.answerStored = false
  return meta
}

const DEFAULT_PREVIEW_KIND = 'fix-patch'

function requestItem(rec: Record<string, unknown>): FixProposalEventItem {
  const meta = asRecord(rec.meta) ?? {}
  const ruleId = asString(rec.ruleId) ?? asString(meta.ruleId) ?? ''
  const blockLabel = asString(rec.blockLabel) ?? asString(meta.blockLabel) ?? ''
  const blockId = asString(rec.blockId)
  return {
    id: asString(rec.id) ?? ruleId,
    label: asString(rec.label) ?? blockLabel,
    ...(blockId ? {blockId} : {}),
    ruleId,
    fixType: asString(rec.fixType) ?? asString(meta.fixType) ?? '',
    blockLabel,
    line: asNumber(rec.line) ?? asNumber(meta.line) ?? 0,
    summary: asString(rec.summary) ?? '',
  }
}

function parseRequest(source: ApprovalSource, data: Record<string, unknown>): ChatEvent | null {
  const proposalId = asString(data.proposalId)
  if (!proposalId) return null
  const rawItems = Array.isArray(data.items) ? data.items : null
  const items: FixProposalEventItem[] = []
  if (rawItems) {
    for (const it of rawItems) {
      const rec = asRecord(it)
      if (rec) items.push(requestItem(rec))
    }
  }
  if (items.length === 0) items.push(requestItem(data))
  const requiredScope = asString(data.requiredScope)
  return {
    kind: 'fix-proposal',
    source,
    proposalId,
    tool: asString(data.tool) ?? '',
    previewKind: asString(data.previewKind) || DEFAULT_PREVIEW_KIND,
    ...(requiredScope ? {requiredScope} : {}),
    items,
  }
}

function parseDecision(source: ApprovalSource, data: Record<string, unknown>): ChatEvent | null {
  const proposalId = asString(data.proposalId)
  const status = asString(data.status)
  if (!proposalId || !status) return null
  const items: FixDecisionEventItem[] = []
  if (Array.isArray(data.items)) {
    for (const it of data.items) {
      const rec = asRecord(it)
      if (!rec) continue
      const id = asString(rec.id)
      const ruleId = asString(rec.ruleId)
      const st = asString(rec.status)
      if ((id || ruleId) && st) {
        items.push({
          ...(id ? {id} : {}),
          ...(ruleId ? {ruleId} : {}),
          status: st,
          message: asString(rec.message) ?? undefined,
        })
      }
    }
  }
  return {
    kind: 'fix-decision',
    source,
    proposalId,
    status,
    message: asString(data.message) ?? undefined,
    items: items.length > 0 ? items : undefined,
  }
}

export function parseChatEvent(raw: unknown): ChatEvent | null {
  const payload = asRecord(raw)
  if (!payload) return null
  if (typeof payload.streamId !== 'string' || !payload.streamId) return null
  const type = asString(payload.type)
  const data = asRecord(payload.data) ?? {}

  switch (type) {
    case 'chunk':
      return {kind: 'chunk', content: asString(data.content) ?? ''}
    case 'done':
      return {
        kind: 'done',
        tokensOut: asNumber(data.tokensOut) ?? 0,
        tokensIn: asNumber(data.tokensIn) ?? 0,
        chunks: asNumber(data.chunks) ?? undefined,
        ...doneMeta(data),
      }
    case 'error': {
      const code = asString(data.code)
      const tokensIn = asNumber(data.tokensIn)
      const tokensOut = asNumber(data.tokensOut)
      const meta = doneMeta(data)
      return {
        kind: 'error',
        message: asString(data.message) ?? 'Unknown error',
        ...(code ? {code} : {}),
        ...(tokensIn !== null ? {tokensIn} : {}),
        ...(tokensOut !== null ? {tokensOut} : {}),
        ...(data.shared === true ? {shared: true} : {}),
        ...(Object.keys(meta).length > 0 ? {meta} : {}),
      }
    }
    case 'heartbeat':
      return {kind: 'heartbeat'}
    case 'tool':
      return {kind: 'tool', label: asString(data.label) ?? asString(data.name) ?? 'Using tool'}
    case 'notice': {
      const notice = asString(data.kind)
      if (!notice) return null
      const dropped = asNumber(data.dropped)
      const keptFrom = asNumber(data.keptFrom)
      const status = notice === NOTICE_KNOWLEDGE ? asKnowledgeStatus(data.status) : null
      return {
        kind: 'notice',
        notice,
        ...(dropped !== null ? {dropped} : {}),
        ...(keptFrom !== null ? {keptFrom} : {}),
        ...(status !== null ? {status} : {}),
      }
    }
    case 'tool_result': {
      const name = asString(data.name)
      if (!name) return null
      return {
        kind: 'tool-result',
        name,
        label: asString(data.label) ?? name,
        ok: data.ok === true,
        durationMs: asNumber(data.durationMs) ?? 0,
        summary: asString(data.summary) ?? '',
      }
    }
    case 'fix_proposal':
      return parseRequest('fix', data)
    case 'fix_decision':
      return parseDecision('fix', data)
    case 'approval_request':
      return parseRequest('approval', data)
    case 'approval_decision':
      return parseDecision('approval', data)
    default:
      return null
  }
}

export function chatEventStreamId(raw: unknown): string | null {
  const payload = asRecord(raw)
  if (!payload) return null
  return typeof payload.streamId === 'string' && payload.streamId ? payload.streamId : null
}

export interface ResumeEvent {
  type: string
  data: Record<string, unknown>
}

export function parseResumeEvents(raw: unknown): ChatEvent[] {
  if (!Array.isArray(raw)) return []
  const out: ChatEvent[] = []
  for (const entry of raw) {
    const rec = asRecord(entry)
    if (!rec) continue
    const type = asString(rec.type)
    if (!type) continue
    const data = asRecord(rec.data) ?? {}
    const parsed = parseChatEvent({streamId: 'replay', type, data})
    if (parsed) out.push(parsed)
  }
  return out
}
