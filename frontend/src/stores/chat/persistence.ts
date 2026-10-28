import {currentUserId} from '../storeRegistry'
import {storableModelLabel} from '@/lib/modelIds'
import type {BlockAnchor} from '@/lib/blockAnchor'
import {isTauri} from '@/platform/guards'
import {MAX_THREADS} from './types'
import type {ChatThread, InFlightTurn, ThreadPinFields} from './types'

export const LEGACY_FLOW_SCOPE = 'flow'

const THREADS_KEY_V1 = 'baki:chatThreads:v1:'
const THREADS_KEY_V2 = 'baki:chatThreads:v2:'
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

function threadsStorageKey(flowId: string): string | null {
  if (isTauri()) return THREADS_KEY_V1 + flowId
  const userId = currentUserId()
  return userId ? `${THREADS_KEY_V2}${userId}:${flowId}` : null
}

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
  }
}

type StoredThread = Pick<
  ChatThread,
  | 'id'
  | 'flowId'
  | 'title'
  | 'createdAt'
  | 'contextBlockId'
  | 'contextBlockAnchor'
  | 'contextBlockLost'
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

const MAX_PIN_PROVIDER_LENGTH = 64
const MAX_PIN_MODEL_LENGTH = 256
const MAX_PIN_LABEL_LENGTH = 200

function isPinProvider(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= MAX_PIN_PROVIDER_LENGTH
}

function readThreadPin(t: Record<string, unknown>): ThreadPinFields {
  const {provider, model, modelLabel} = t
  if (!isPinProvider(provider)) return {}
  if (typeof model !== 'string' || !model.trim() || model.length > MAX_PIN_MODEL_LENGTH) return {}
  const pin: ThreadPinFields = {provider, model}
  const label =
    typeof modelLabel === 'string' && modelLabel.length <= MAX_PIN_LABEL_LENGTH
      ? storableModelLabel(modelLabel, model)
      : undefined
  if (label) pin.modelLabel = label
  return pin
}

function readAnchor(raw: unknown): BlockAnchor | null {
  if (!raw || typeof raw !== 'object') return null
  const {subflow, rawType, name, ordinal, count} = raw as Record<string, unknown>
  if (typeof subflow !== 'string' || typeof rawType !== 'string' || typeof name !== 'string') return null
  if (typeof ordinal !== 'number' || typeof count !== 'number') return null
  if (!Number.isInteger(ordinal) || !Number.isInteger(count)) return null
  return {subflow, rawType, name, ordinal, count}
}

function readInFlight(raw: unknown): InFlightTurn | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const r = raw as Record<string, unknown>
  if (typeof r.streamId !== 'string' || !UUID_RE.test(r.streamId)) return undefined
  if (!isPinProvider(r.provider)) return undefined
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
      contextBlockAnchor: t.contextBlockAnchor ?? undefined,
      contextBlockLost: t.contextBlockLost,
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
      contextBlockAnchor: readAnchor(t.contextBlockAnchor),
      selectedSourceFiles: Array.isArray(t.selectedSourceFiles)
        ? t.selectedSourceFiles.filter((f): f is string => typeof f === 'string')
        : [],
      tokensIn: typeof t.tokensIn === 'number' ? t.tokensIn : 0,
      tokensOut: typeof t.tokensOut === 'number' ? t.tokensOut : 0,
      // false is a choice too (tools are on by default, R-frontend-6); only a missing value means "the default".
      useTools: typeof t.useTools === 'boolean' ? t.useTools : undefined,
      usedTools: t.usedTools === true ? true : undefined,
      appliedFixes: t.appliedFixes === true ? true : undefined,
      ...readThreadPin(t),
    }
    out.push(thread)
    if (typeof t.contextBlockLost === 'string') thread.contextBlockLost = t.contextBlockLost
    const inFlight = readInFlight(t.inFlight)
    if (inFlight) thread.inFlight = inFlight
    if (out.length >= MAX_THREADS) break
  }
  return out
}
