import {request, requestValidated} from './client'
import {isReplicaUnreachable, retryAfterHint} from '@/lib/chatErrors'
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

export interface BeginStreamResult {
  status: 'ok' | 'finished'
  text?: string
  done?: boolean
  error?: string
  errorCode?: string
  errorShared?: boolean
  tokensIn?: number
  tokensOut?: number
  servedModel?: string
}

// A stop that landed on a server other than the one running the stream is relayed to it; when that fails (503
// REPLICA_UNREACHABLE) nothing was stopped, so it is sent again after the wait the server named, twice, before the
// caller hears of it.
const CANCEL_RETRIES = 2
const CANCEL_RETRY_DEFAULT_S = 2

async function cancelWithRetry(id: string): Promise<void> {
  for (let attempt = 0; ; attempt++) {
    try {
      await request<void>('/api/chat/cancel', {body: {id}})
      return
    } catch (err) {
      if (!isReplicaUnreachable(err) || attempt >= CANCEL_RETRIES) throw err
      const seconds = retryAfterHint(err) ?? CANCEL_RETRY_DEFAULT_S
      await new Promise(resolve => setTimeout(resolve, Math.min(seconds, 10) * 1000))
    }
  }
}

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> => request('/api/chat/stream', {body: req}),

  beginStream: (id: string): Promise<BeginStreamResult> => request('/api/chat/begin', {body: {id}}),

  cancelStream: (id: string): Promise<void> => cancelWithRetry(id),

  respondFixDecision: (
    streamId: string,
    proposalId: string,
    approved: boolean,
    excludedItemIndices?: number[],
  ): Promise<void> => request('/api/chat/fix-decision', {body: {streamId, proposalId, approved, excludedItemIndices}}),

  respondApprovalDecision: (
    streamId: string,
    proposalId: string,
    approved: boolean,
    excludedItemIndices?: number[],
  ): Promise<void> =>
    request('/api/chat/approval-decision', {body: {streamId, proposalId, approved, excludedItemIndices}}),

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
