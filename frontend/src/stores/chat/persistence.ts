import {currentUserId} from '../storeRegistry'
import {storableModelLabel} from '@/lib/modelIds'
import {isTauri} from '@/platform/guards'
import {MAX_THREADS, isProviderId} from './types'
import type {ChatThread, InFlightTurn, ThreadPinFields} from './types'

// LEGACY_FLOW_SCOPE is the pre-thread conversation key. A flow whose threads
// were never persisted (first open after upgrade, or a different browser)
// restores its history from here once, then re-saves it under the new
// thread's own key.
export const LEGACY_FLOW_SCOPE = 'flow'

// Thread lists persist per flow in localStorage so a reload restores the same
// threads — and therefore the same conversation keys. Without this every
// reload minted a fresh thread id and the previous thread's history became
// unreachable. Conversations themselves stay server-side; only the thread
// metadata is stored here.
//
// Keys are per USER in the web app: v2:<userId>:<flowId>. The v1 key was
// per flow only, so on a shared browser the next account to open the same flow
// hydrated the previous account's thread ids (and titles, i.e. the start of
// each first prompt) — and the server lets any flow viewer read a
// conversation by key. The desktop app is single-user and keeps its v1 key.
const THREADS_KEY_V1 = 'baki:chatThreads:v1:'
const THREADS_KEY_V2 = 'baki:chatThreads:v2:'
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// threadsStorageKey returns where a flow's thread list lives, or null when it
// must not be stored at all (web app with no signed-in user).
function threadsStorageKey(flowId: string): string | null {
  if (isTauri()) return THREADS_KEY_V1 + flowId
  const userId = currentUserId()
  return userId ? `${THREADS_KEY_V2}${userId}:${flowId}` : null
}

// purgeLegacyThreadKeys drops the unscoped v1 entries in the web app. They
// record no owner, so they cannot be handed to whoever signs in next; their
// conversations stay on the server. Runs on every thread-list read (a flow
// open — a key scan is cheap) and on logout.
export function purgeLegacyThreadKeys(): void {
  if (isTauri()) return
  try {
    const stale: string[] = []
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (k && k.startsWith(THREADS_KEY_V1)) stale.push(k)
    }
    for (const k of stale) localStorage.removeItem(k)
  } catch {
    // Storage unavailable: nothing to purge.
  }
}

type StoredThread = Pick<
  ChatThread,
  | 'id'
  | 'flowId'
  | 'title'
  | 'createdAt'
  | 'contextBlockId'
  | 'selectedSourceFiles'
  | 'tokensIn'
  | 'tokensOut'
  | 'useTools'
  | 'usedTools'
  | 'appliedFixes'
  | 'provider'
  | 'model'
  | 'modelLabel'
  | 'inFlight'
>

// A stored pin's strings are capped: a registry id is "m/" plus 10
// characters and a display name at most 64 on the server, a catalogue id a
// model name — anything longer did not come from this app.
const MAX_PIN_MODEL_LENGTH = 256
const MAX_PIN_LABEL_LENGTH = 200

// readThreadPin validates a stored thread's model pin. Anything off — an
// unknown provider, a missing or oversized model — loads the thread
// unpinned (as every thread from before per-thread memory loads), and
// lib/modelSelection resolves it; a bad label alone is just dropped (as is
// one that is only an id — see storableModelLabel).
function readThreadPin(t: Record<string, unknown>): ThreadPinFields {
  const {provider, model, modelLabel} = t
  if (!isProviderId(provider)) return {}
  if (typeof model !== 'string' || !model.trim() || model.length > MAX_PIN_MODEL_LENGTH) return {}
  const pin: ThreadPinFields = {provider, model}
  const label =
    typeof modelLabel === 'string' && modelLabel.length <= MAX_PIN_LABEL_LENGTH
      ? storableModelLabel(modelLabel, model)
      : undefined
  if (label) pin.modelLabel = label
  return pin
}

// readInFlight validates a stored thread's in-flight turn record. Anything
// off drops the record: a reload then re-attaches the stream unlabelled
// rather than labelled with a guess. Stream and message ids are the
// client's own uuids; the model may be '' (the server's default).
function readInFlight(raw: unknown): InFlightTurn | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const r = raw as Record<string, unknown>
  if (typeof r.streamId !== 'string' || !UUID_RE.test(r.streamId)) return undefined
  if (!isProviderId(r.provider)) return undefined
  if (typeof r.model !== 'string' || r.model.length > MAX_PIN_MODEL_LENGTH) return undefined
  const turn: InFlightTurn = {streamId: r.streamId, provider: r.provider, model: r.model}
  if (typeof r.messageId === 'string' && UUID_RE.test(r.messageId)) turn.messageId = r.messageId
  const label =
    typeof r.modelLabel === 'string' && r.modelLabel.length <= MAX_PIN_LABEL_LENGTH
      ? storableModelLabel(r.modelLabel, r.model)
      : undefined
  if (label) turn.modelLabel = label
  return turn
}

export function persistFlowThreads(flowId: string, threads: ChatThread[]): void {
  const key = threadsStorageKey(flowId)
  if (!key) return
  const mine: StoredThread[] = threads
    .filter(t => t.flowId === flowId)
    .map(t => ({
      id: t.id,
      flowId: t.flowId,
      title: t.title,
      createdAt: t.createdAt,
      contextBlockId: t.contextBlockId,
      selectedSourceFiles: t.selectedSourceFiles,
      tokensIn: t.tokensIn,
      tokensOut: t.tokensOut,
      useTools: t.useTools,
      usedTools: t.usedTools,
      appliedFixes: t.appliedFixes,
      provider: t.provider,
      model: t.model,
      modelLabel: t.modelLabel,
      inFlight: t.inFlight,
    }))
  try {
    if (mine.length === 0) localStorage.removeItem(key)
    else localStorage.setItem(key, JSON.stringify(mine))
  } catch {
    // Storage may be unavailable (private mode, quota); threads then last the session.
  }
}

export function readStoredThreads(flowId: string): ChatThread[] {
  purgeLegacyThreadKeys()
  const key = threadsStorageKey(flowId)
  if (!key) return []
  let parsed: unknown
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return []
    parsed = JSON.parse(raw)
  } catch {
    // Storage unavailable or corrupt entry: start with no threads.
    return []
  }
  if (!Array.isArray(parsed)) return []
  const out: ChatThread[] = []
  for (const item of parsed) {
    if (!item || typeof item !== 'object') continue
    const t = item as Record<string, unknown>
    if (typeof t.id !== 'string' || !UUID_RE.test(t.id) || t.flowId !== flowId) continue
    const thread: ChatThread = {
      id: t.id,
      flowId,
      title: typeof t.title === 'string' ? t.title : '',
      createdAt: typeof t.createdAt === 'string' ? t.createdAt : new Date().toISOString(),
      contextBlockId: typeof t.contextBlockId === 'string' ? t.contextBlockId : null,
      selectedSourceFiles: Array.isArray(t.selectedSourceFiles)
        ? t.selectedSourceFiles.filter((f): f is string => typeof f === 'string')
        : [],
      tokensIn: typeof t.tokensIn === 'number' ? t.tokensIn : 0,
      tokensOut: typeof t.tokensOut === 'number' ? t.tokensOut : 0,
      useTools: t.useTools === true ? true : undefined,
      usedTools: t.usedTools === true ? true : undefined,
      appliedFixes: t.appliedFixes === true ? true : undefined,
      ...readThreadPin(t),
    }
    out.push(thread)
    const inFlight = readInFlight(t.inFlight)
    if (inFlight) thread.inFlight = inFlight
    if (out.length >= MAX_THREADS) break
  }
  return out
}
