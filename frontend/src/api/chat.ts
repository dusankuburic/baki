import {request} from './client'
import {parseConversationPayload} from '@/lib/chatMessage'
import type {ChatRequest, ChatMessage, ContextPreview} from '@/types'

// ConversationLoad is a /api/chat/get response: the validated message list
// plus, while the server tracks an in-flight (or still-resumable) stream for
// this conversation, the id a reloaded client can re-attach to. The
// backend historically answers a bare array — that shape carries no id.
export interface ConversationLoad {
  messages: ChatMessage[]
  activeStreamId?: string
}

// parseConversationLoad keeps parseConversationPayload as the validator (an
// unexpected payload shape must still count as a failed load) and only lifts
// activeStreamId off the object form. `{"messages": null}` (a server that
// serialised an empty Go slice as null) is an empty history, not a failure.
function parseConversationLoad(raw: unknown): ConversationLoad {
  const isObject = raw != null && typeof raw === 'object' && !Array.isArray(raw)
  const messages = isObject && (raw as {messages?: unknown}).messages === null ? [] : parseConversationPayload(raw)
  if (isObject) {
    const activeStreamId = (raw as {activeStreamId?: unknown}).activeStreamId
    if (typeof activeStreamId === 'string' && activeStreamId) return {messages, activeStreamId}
  }
  return {messages}
}

// BeginStreamResult reports whether the stream is live (events arrive over
// SSE) or already finished — a fail-fast pre-stream error emits its terminal
// event before the client subscribes, so /begin returns that state directly.
export interface BeginStreamResult {
  status: 'ok' | 'finished'
  text?: string
  done?: boolean
  error?: string
  // Machine-readable class of `error` (AI_BUDGET_EXCEEDED, …), when known.
  errorCode?: string
  // True when the failure came from the organisation's shared credential
  // — the error bubble then also offers the Organizations section.
  errorShared?: boolean
  tokensIn?: number
  tokensOut?: number
  // The model that answered, when the provider resolved an alias, and the
  // USD per 1M token rates the turn was billed at (see DoneMeta in
  // lib/chatEvent). Optional: older servers send only servedModel.
  servedModel?: string
  inputCostPerM?: number
  outputCostPerM?: number
}

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> => request('/api/chat/stream', {body: req}),

  beginStream: (id: string): Promise<BeginStreamResult> => request('/api/chat/begin', {body: {id}}),

  cancelStream: (id: string): Promise<void> => request('/api/chat/cancel', {body: {id}}),

  // Respond to an apply_fix approval prompt pending in one of the caller's
  // streams (the AI proposed a source fix; the user clicked Approve/Dismiss).
  respondFixDecision: (
    streamId: string,
    proposalId: string,
    approved: boolean,
    excludedItemIndices?: number[],
  ): Promise<void> => request('/api/chat/fix-decision', {body: {streamId, proposalId, approved, excludedItemIndices}}),

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
    // Same as BeginStreamResult: the served model and billed rates of a
    // finished turn (or, with error, a BILLED failed one), when known.
    servedModel?: string
    inputCostPerM?: number
    outputCostPerM?: number
    events?: {type: string; data: Record<string, unknown>}[]
  }> => request('/api/chat/resume', {body: {id, from}}),

  // The conversation endpoints are keyed by (flowId, conversation key). The key
  // travels in the wire field historically named `provider`; it is the
  // thread's id (conversationKey), not a provider id.
  //
  // Resolves the validated message list; rejects when the response is not a
  // conversation (see parseConversationPayload) so callers treat it as a
  // failed load rather than an empty history.
  getConversation: (flowId: string, conversationKey: string): Promise<ConversationLoad> =>
    request<unknown>('/api/chat/get', {body: {flowId, provider: conversationKey}}).then(parseConversationLoad),

  saveConversation: (flowId: string, conversationKey: string, messages: ChatMessage[]): Promise<void> =>
    request('/api/chat/save', {body: {flowId, provider: conversationKey, messages}}),

  clearConversation: (flowId: string, conversationKey: string): Promise<void> =>
    request('/api/chat/clear', {body: {flowId, provider: conversationKey}}),

  getSuggestedPrompts: (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    request('/api/chat/suggested-prompts', {body: {hasBlock, hasFindings: hasFindings ?? false}}),

  previewContext: (req: ChatRequest): Promise<ContextPreview> => request('/api/chat/preview-context', {body: req}),
}
