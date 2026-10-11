
export interface DoneMeta {
  servedModel?: string
  inputCostPerM?: number
  outputCostPerM?: number
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

export interface FixProposalEventItem {
  id: string
  label: string
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

function doneMeta(data: Record<string, unknown>): DoneMeta {
  const meta: DoneMeta = {}
  const servedModel = asString(data.servedModel)
  if (servedModel) meta.servedModel = servedModel
  const inputCostPerM = asNumber(data.inputCostPerM)
  if (inputCostPerM !== null) meta.inputCostPerM = inputCostPerM
  const outputCostPerM = asNumber(data.outputCostPerM)
  if (outputCostPerM !== null) meta.outputCostPerM = outputCostPerM
  return meta
}

const DEFAULT_PREVIEW_KIND = 'fix-patch'

function requestItem(rec: Record<string, unknown>): FixProposalEventItem {
  const meta = asRecord(rec.meta) ?? {}
  const ruleId = asString(rec.ruleId) ?? asString(meta.ruleId) ?? ''
  const blockLabel = asString(rec.blockLabel) ?? asString(meta.blockLabel) ?? ''
  return {
    id: asString(rec.id) ?? ruleId,
    label: asString(rec.label) ?? blockLabel,
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
