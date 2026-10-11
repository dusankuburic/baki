import {request, requestValidated} from './client'
import {getSuggestedPromptsSchema} from './schemas'
import {parseConversationPayload} from '@/lib/chatMessage'
import type {ChatRequest, ChatMessage, ContextPreview} from '@/types'

export interface ConversationLoad {
  messages: ChatMessage[]
  activeStreamId?: string
}

function parseConversationLoad(raw: unknown): ConversationLoad {
  const isObject = raw != null && typeof raw === 'object' && !Array.isArray(raw)
  const messages = isObject && (raw as {messages?: unknown}).messages === null ? [] : parseConversationPayload(raw)
  if (isObject) {
    const activeStreamId = (raw as {activeStreamId?: unknown}).activeStreamId
    if (typeof activeStreamId === 'string' && activeStreamId) return {messages, activeStreamId}
  }
  return {messages}
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
  inputCostPerM?: number
  outputCostPerM?: number
}

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> => request('/api/chat/stream', {body: req}),

  beginStream: (id: string): Promise<BeginStreamResult> => request('/api/chat/begin', {body: {id}}),

  cancelStream: (id: string): Promise<void> => request('/api/chat/cancel', {body: {id}}),

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
    inputCostPerM?: number
    outputCostPerM?: number
    events?: {type: string; data: Record<string, unknown>}[]
  }> => request('/api/chat/resume', {body: {id, from}}),

  getConversation: (flowId: string, conversationKey: string): Promise<ConversationLoad> =>
    request<unknown>('/api/chat/get', {body: {flowId, provider: conversationKey}}).then(parseConversationLoad),

  saveConversation: (flowId: string, conversationKey: string, messages: ChatMessage[]): Promise<void> =>
    request('/api/chat/save', {body: {flowId, provider: conversationKey, messages}}),

  clearConversation: (flowId: string, conversationKey: string): Promise<void> =>
    request('/api/chat/clear', {body: {flowId, provider: conversationKey}}),

  getSuggestedPrompts: async (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    requestValidated('/api/chat/suggested-prompts', await getSuggestedPromptsSchema(), {
      body: {hasBlock, hasFindings: hasFindings ?? false},
    }),

  previewContext: (req: ChatRequest): Promise<ContextPreview> => request('/api/chat/preview-context', {body: req}),
}
