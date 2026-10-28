import {readFileSync, readdirSync} from 'node:fs'
import {resolve} from 'node:path'

/**
 * The backend's chat event goldens (internal/service/testdata/chat_events, R-testing-3): the SSE envelopes and the
 * resume body exactly as the server sends them. Tests replay these instead of hand-written payloads, so a field the
 * backend renames or drops fails here.
 */
// Relative to the vitest root (frontend/): the vmThreads pool gives modules no file URL.
const DIR = resolve(process.cwd(), '../internal/service/testdata/chat_events') + '/'

export interface ChatEventEnvelope {
  streamId: string
  type: string
  data: Record<string, unknown>
}

/** The names of the golden files, without the .json extension. */
export function chatEventGoldenNames(): string[] {
  return readdirSync(DIR)
    .filter(f => f.endsWith('.json'))
    .map(f => f.slice(0, -'.json'.length))
    .sort()
}

/** One golden, parsed; envelopes are re-addressed to streamId when given. */
export function chatEventGolden<T = ChatEventEnvelope>(name: string, streamId?: string): T {
  const value = JSON.parse(readFileSync(DIR + name + '.json', 'utf8')) as T
  if (streamId && value && typeof value === 'object' && 'streamId' in value) {
    return {...value, streamId} as T
  }
  return value
}
