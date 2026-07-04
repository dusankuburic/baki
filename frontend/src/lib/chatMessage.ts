import type {ChatMessage} from '@/types'

// Known message roles the backend may send. A payload with any other role is
// coerced to 'assistant' (the safest non-actionable default) so a malformed or
// unexpected payload can't inject an unknown role into the store.
const KNOWN_ROLES = new Set(['user', 'assistant', 'system'])

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

    const role = typeof m.role === 'string' && KNOWN_ROLES.has(m.role)
        ? (m.role as ChatMessage['role'])
        : 'assistant'

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
    if (typeof m.contextSubflowId === 'string' && m.contextSubflowId) out.contextSubflowId = m.contextSubflowId
    if (typeof m.tokensIn === 'number') out.tokensIn = m.tokensIn
    if (typeof m.tokensOut === 'number') out.tokensOut = m.tokensOut
    if (typeof m.provider === 'string' && m.provider) out.provider = m.provider as ChatMessage['provider']
    if (typeof m.model === 'string' && m.model) out.model = m.model
    if (typeof m.finishReason === 'string') out.finishReason = m.finishReason as ChatMessage['finishReason']
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
