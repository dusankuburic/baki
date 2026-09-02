// Typed parser for `chat:event` SSE payloads (the backend's chat stream
// envelope — mirrors the writer in internal/api/chat_events). Previously these
// payloads were unvalidated `as` casts, so a malformed frame (proxy error
// page, truncated JSON, a different event's shape) flowed straight into
// chat-store writes. parseChatEvent returns a discriminated union or null;
// null means "not a chat:event for this shape" and callers drop the frame.
//
// Deliberately hand-rolled and synchronous rather than a zod schema: chunk
// frames arrive at streaming cadence and feed a RAF-coalesced renderer, and
// an async (memoized-import) validator would add an await boundary on the hot
// path. This stays allocation-light and order-preserving.

export type ChatEvent =
  | {kind: 'chunk'; content: string}
  | {kind: 'done'; tokensOut: number; tokensIn: number; chunks?: number}
  | {kind: 'error'; message: string}
  | {kind: 'tool'; label: string}
  | {kind: 'tool-result'; name: string; label: string; ok: boolean; durationMs: number; summary: string}
  | {kind: 'fix-proposal'; proposalId: string; items: FixProposalEventItem[]}
  | {kind: 'fix-decision'; proposalId: string; status: string; message?: string; items?: {ruleId: string; status: string; message?: string}[]}

// FixProposalEventItem is one fix inside a proposal event (single-fix events
// carry one item synthesized from the flat fields).
export interface FixProposalEventItem {
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
}

// FixProposalPayload is the parsed fix-proposal event (the apply_fix /
// apply_fixes approval prompt's content).
export type FixProposalPayload = Extract<ChatEvent, {kind: 'fix-proposal'}>

// ToolResultPayload is the parsed tool_result event — one finished tool
// execution in the transparency trail.
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
      }
    case 'error':
      return {kind: 'error', message: asString(data.message) ?? 'Unknown error'}
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
    case 'fix_proposal': {
      const proposalId = asString(data.proposalId)
      if (!proposalId) return null
      // Batch events carry items[]; single-fix events carry flat fields —
      // normalize both to items[] (single = one item).
      const rawItems = Array.isArray(data.items) ? data.items : null
      const items: FixProposalEventItem[] = []
      if (rawItems) {
        for (const it of rawItems) {
          const rec = asRecord(it)
          if (!rec) continue
          items.push({
            ruleId: asString(rec.ruleId) ?? '',
            fixType: asString(rec.fixType) ?? '',
            blockLabel: asString(rec.blockLabel) ?? '',
            line: asNumber(rec.line) ?? 0,
            summary: asString(rec.summary) ?? '',
          })
        }
      }
      if (items.length === 0) {
        items.push({
          ruleId: asString(data.ruleId) ?? '',
          fixType: asString(data.fixType) ?? '',
          blockLabel: asString(data.blockLabel) ?? '',
          line: asNumber(data.line) ?? 0,
          summary: asString(data.summary) ?? '',
        })
      }
      return {kind: 'fix-proposal', proposalId, items}
    }
    case 'fix_decision': {
      const proposalId = asString(data.proposalId)
      const status = asString(data.status)
      if (!proposalId || !status) return null
      const items: {ruleId: string; status: string; message?: string}[] = []
      if (Array.isArray(data.items)) {
        for (const it of data.items) {
          const rec = asRecord(it)
          if (!rec) continue
          const rid = asString(rec.ruleId)
          const st = asString(rec.status)
          if (rid && st) items.push({ruleId: rid, status: st, message: asString(rec.message) ?? undefined})
        }
      }
      return {kind: 'fix-decision', proposalId, status, message: asString(data.message) ?? undefined, items: items.length > 0 ? items : undefined}
    }
    default:
      return null
  }
}

/** The streamId a chat:event envelope is addressed to, or null if malformed. */
export function chatEventStreamId(raw: unknown): string | null {
  const payload = asRecord(raw)
  if (!payload) return null
  return typeof payload.streamId === 'string' && payload.streamId ? payload.streamId : null
}

// ResumeEvent is one journaled agentic event from a resume response — the
// same {type, data} shape a chat:event envelope carries in `data`-less form.
export interface ResumeEvent {
  type: string
  data: Record<string, unknown>
}

/** Parse a resume response's journal into replayable ChatEvents (drops unknown). */
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
