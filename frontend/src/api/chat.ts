import {request, requestValidated} from './client'
import {isReplicaUnreachable, retryAfterHint} from '@/lib/chatErrors'
import {isRateLimited} from '@/lib/streamControl'
import {getSuggestedPromptsSchema} from './schemas'
import {parseConversationPayload} from '@/lib/chatMessage'
import type {ChatRequest, ChatMessage, ContextPreview} from '@/types'

export interface ConversationLoad {
  messages: ChatMessage[]
  activeStreamId?: string
  /** The stored conversation's version (0: nothing stored yet). Absent from a server without versions. */
  version?: number
}

/** What a save reports: the new version when the server versions conversations (absent from an older server). */
export interface ConversationSaveResult {
  version?: number
}

// A version is a non-negative integer; anything else (a server without versions, a garbled body) reads as unknown.
function parseVersion(raw: unknown): number | undefined {
  return typeof raw === 'number' && Number.isSafeInteger(raw) && raw >= 0 ? raw : undefined
}

/** One of the caller's threads of a flow, as the server's thread index lists it (R-frontend-c26). */
export interface ServerThread {
  /** The conversation key: the thread's id on the client. */
  id: string
  title: string
  createdAt: string
  updatedAt: string
  messageCount: number
  /** The stored conversation's version: the base of the next save or clear. */
  version: number
}

export interface ServerThreadPage {
  items: ServerThread[]
  nextCursor?: string
}

function parseServerThread(raw: unknown): ServerThread | null {
  if (!raw || typeof raw !== 'object') return null
  const r = raw as Record<string, unknown>
  if (typeof r.id !== 'string' || !r.id) return null
  const str = (v: unknown) => (typeof v === 'string' ? v : '')
  const num = (v: unknown) => (typeof v === 'number' && Number.isSafeInteger(v) && v >= 0 ? v : 0)
  return {
    id: r.id,
    title: str(r.title),
    createdAt: str(r.createdAt),
    updatedAt: str(r.updatedAt),
    messageCount: num(r.messageCount),
    version: num(r.version),
  }
}

function parseServerThreadPage(raw: unknown): ServerThreadPage {
  const r = raw && typeof raw === 'object' ? (raw as {items?: unknown; nextCursor?: unknown}) : {}
  const items = Array.isArray(r.items) ? r.items.map(parseServerThread).filter((t): t is ServerThread => !!t) : []
  return typeof r.nextCursor === 'string' && r.nextCursor ? {items, nextCursor: r.nextCursor} : {items}
}

function parseConversationLoad(raw: unknown): ConversationLoad {
  const isObject = raw != null && typeof raw === 'object' && !Array.isArray(raw)
  const messages = isObject && (raw as {messages?: unknown}).messages === null ? [] : parseConversationPayload(raw)
  const load: ConversationLoad = {messages}
  if (isObject) {
    const activeStreamId = (raw as {activeStreamId?: unknown}).activeStreamId
    if (typeof activeStreamId === 'string' && activeStreamId) load.activeStreamId = activeStreamId
    const version = parseVersion((raw as {version?: unknown}).version)
    if (version !== undefined) load.version = version
  }
  return load
}

// A stop that landed on a server other than the one running the stream is relayed to it; when that fails (503
// REPLICA_UNREACHABLE) nothing was stopped, so it is sent again after the wait the server named, twice, before the
// caller hears of it.
const CANCEL_RETRIES = 2
const CANCEL_RETRY_DEFAULT_S = 2
// A stream-control command a rate limit refused (429) never reached the stream (R-streaming-c20): it is sent again
// after the wait the 429's Retry-After names, more patiently than a relay failure, since a stop that does not get
// through leaves a billed stream running.
const RATE_LIMITED_RETRIES = 5
const RATE_LIMITED_RETRY_DEFAULT_S = 1
const RETRY_WAIT_MAX_S = 10

async function sendControl<T>(send: () => Promise<T>, relayRetries: number): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await send()
    } catch (err) {
      const limited = isRateLimited(err)
      const retries = limited ? RATE_LIMITED_RETRIES : isReplicaUnreachable(err) ? relayRetries : 0
      if (attempt >= retries) throw err
      const seconds = retryAfterHint(err) ?? (limited ? RATE_LIMITED_RETRY_DEFAULT_S : CANCEL_RETRY_DEFAULT_S)
      await new Promise(resolve => setTimeout(resolve, Math.min(seconds, RETRY_WAIT_MAX_S) * 1000))
    }
  }
}

async function cancelWithRetry(id: string): Promise<void> {
  await sendControl(() => request<void>('/api/chat/cancel', {body: {id}}), CANCEL_RETRIES)
}

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> => request('/api/chat/stream', {body: req}),

  cancelStream: (id: string): Promise<void> => cancelWithRetry(id),

  respondFixDecision: (
    streamId: string,
    proposalId: string,
    approved: boolean,
    excludedItemIndices?: number[],
  ): Promise<void> =>
    // A relay failure is the caller's to offer again (the card goes back to pending); a rate limit is retried here.
    sendControl(
      () => request('/api/chat/fix-decision', {body: {streamId, proposalId, approved, excludedItemIndices}}),
      0,
    ),

  respondApprovalDecision: (
    streamId: string,
    proposalId: string,
    approved: boolean,
    excludedItemIndices?: number[],
  ): Promise<void> =>
    sendControl(
      () => request('/api/chat/approval-decision', {body: {streamId, proposalId, approved, excludedItemIndices}}),
      0,
    ),

  resumeStream: (
    id: string,
    from = 0,
  ): Promise<{
    text: string
    done: boolean
    error: string
    errorCode?: string
    errorShared?: boolean
    tokensIn: number
    tokensOut: number
    servedModel?: string
    events?: {type: string; data: Record<string, unknown>}[]
    /** False when the server could not store the finished answer: the client saves the turn (R-tool-loop-20). */
    answerStored?: false
  }> => request('/api/chat/resume', {body: {id, from}}),

  getConversation: (flowId: string, conversationKey: string): Promise<ConversationLoad> =>
    request<unknown>('/api/chat/get', {body: {flowId, provider: conversationKey}}).then(parseConversationLoad),

  /** baseVersion: the version the messages were built on; the server refuses a stale one (409 CONVERSATION_CONFLICT). */
  saveConversation: (
    flowId: string,
    conversationKey: string,
    messages: ChatMessage[],
    baseVersion?: number,
  ): Promise<ConversationSaveResult> =>
    request<unknown>('/api/chat/save', {
      body: {flowId, provider: conversationKey, messages, ...(baseVersion !== undefined ? {baseVersion} : {})},
    }).then(raw => {
      const version = parseVersion((raw as {version?: unknown} | null | undefined)?.version)
      return version === undefined ? {} : {version}
    }),

  /** The caller's own threads of a flow, newest first (empty where conversations are not stored server-side). */
  listThreads: (flowId: string, page: {limit?: number; cursor?: string} = {}): Promise<ServerThreadPage> =>
    request<unknown>('/api/chat/threads', {body: {flowId, ...page}}).then(parseServerThreadPage),

  /**
   * Names a thread for every browser's index ("" removes the name). A thread not stored yet is stored empty with
   * the name, at a version the answer carries: a client still at 0 adopts it as its base.
   */
  setThreadTitle: (flowId: string, conversationId: string, title: string): Promise<{version?: number}> =>
    request<unknown>('/api/chat/title', {body: {flowId, conversationId, title}}).then(raw => {
      const version = parseVersion((raw as {version?: unknown} | null | undefined)?.version)
      return version === undefined ? {} : {version}
    }),

  clearConversation: (flowId: string, conversationKey: string, baseVersion?: number): Promise<void> =>
    request('/api/chat/clear', {
      body: {flowId, provider: conversationKey, ...(baseVersion !== undefined ? {baseVersion} : {})},
    }),

  getSuggestedPrompts: async (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    requestValidated('/api/chat/suggested-prompts', await getSuggestedPromptsSchema(), {
      body: {hasBlock, hasFindings: hasFindings ?? false},
    }),

  previewContext: (req: ChatRequest): Promise<ContextPreview> => request('/api/chat/preview-context', {body: req}),
}
