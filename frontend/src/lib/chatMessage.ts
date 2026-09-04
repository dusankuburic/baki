import type {ChatMessage, FixItemSnapshot, FixProposalSnapshot, ToolCallRecord} from '@/types'

// Known message roles the backend may send. A payload with any other role is
// coerced to 'assistant' (the safest non-actionable default) so a malformed or
// unexpected payload can't inject an unknown role into the store.
const KNOWN_ROLES = new Set(['user', 'assistant', 'system'])

// parseToolCalls validates the transparency trail attached to an assistant
// message. The trail is persisted with the conversation (useChatStreamEngine
// saves it on done), so it has to survive the read boundary too — without
// this, reopening a flow silently dropped every ToolTrail and FixOutcomeStrip
// that had been saved.
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

// A persisted proposal must carry an id and a resolved status; everything else
// is descriptive copy that defaults to empty rather than dropping the record
// (an outcome strip with a blank label still tells the user what happened).
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

// parseChatMessage coerces an untrusted backend payload into a ChatMessage,
// validating and defaulting each field instead of trusting the shape via `as`.
// It returns null only when the payload lacks the identity/timestamp a stored
// message needs; an empty content (e.g. an interrupted assistant turn) is kept.
//
// This is the type-safety guard for backend conversation responses
// (docs/IMPROVEMENTS.md, F5): it replaces unchecked `as ChatMessage` casts at
// the read boundary so a malformed server/fixture message can't reach the chat
// store with a missing id, an unknown role, or a non-string content.
export function parseChatMessage(raw: unknown): ChatMessage | null {
  if (!raw || typeof raw !== 'object') return null
  const m = raw as Record<string, unknown>

  const id = typeof m.id === 'string' ? m.id : ''
  const timestamp = typeof m.timestamp === 'string' ? m.timestamp : ''
  // id + timestamp are required to append/reconcile a message; content may be
  // empty but must be a string so renderers can call .slice/.length safely.
  if (!id || !timestamp) return null

  const role = typeof m.role === 'string' && KNOWN_ROLES.has(m.role) ? (m.role as ChatMessage['role']) : 'assistant'

  const out: ChatMessage = {
    id,
    role,
    content: typeof m.content === 'string' ? m.content : '',
    timestamp,
  }
  // Optional fields are attached only when present with the right type, so the
  // parsed message never carries an undefined-valued key the store would have
  // to defend against.
  if (typeof m.contextBlockId === 'string' && m.contextBlockId) out.contextBlockId = m.contextBlockId
  if (typeof m.tokensIn === 'number') out.tokensIn = m.tokensIn
  if (typeof m.tokensOut === 'number') out.tokensOut = m.tokensOut
  if (typeof m.provider === 'string' && m.provider) out.provider = m.provider as ChatMessage['provider']
  if (typeof m.model === 'string' && m.model) out.model = m.model
  if (typeof m.finishReason === 'string') out.finishReason = m.finishReason as ChatMessage['finishReason']

  // Agentic trail. Saved by useChatStreamEngine on done; restored here so a
  // reopened conversation still shows which tools ran and how each fix
  // proposal resolved.
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
  return out
}

// parseChatMessages validates a backend conversation's message list, dropping
// any entries that fail parseChatMessage. Callers can append the result without
// a per-message guard. A non-array or empty payload yields an empty list.
export function parseChatMessages(raw: unknown): ChatMessage[] {
  if (!Array.isArray(raw)) return []
  const out: ChatMessage[] = []
  for (const m of raw) {
    const parsed = parseChatMessage(m)
    if (parsed) out.push(parsed)
  }
  return out
}
