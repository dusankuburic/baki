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
