import {request} from './client'
import type {ChatRequest, ChatMessage, ConversationFile, ContextPreview} from '@/types'

// BeginStreamResult reports whether the stream is live (events arrive over
// SSE) or already finished — a fail-fast pre-stream error emits its terminal
// event before the client subscribes, so /begin returns that state directly.
export interface BeginStreamResult {
  status: 'ok' | 'finished'
  text?: string
  done?: boolean
  error?: string
  tokensIn?: number
  tokensOut?: number
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
    tokensIn: number
    tokensOut: number
    events?: {type: string; data: Record<string, unknown>}[]
  }> => request('/api/chat/resume', {body: {id, from}}),

  getConversation: (flowId: string, provider: string): Promise<ConversationFile> =>
    request('/api/chat/get', {body: {flowId, provider}}),

  saveConversation: (flowId: string, provider: string, messages: ChatMessage[]): Promise<void> =>
    request('/api/chat/save', {body: {flowId, provider, messages}}),

  clearConversation: (flowId: string, provider: string): Promise<void> =>
    request('/api/chat/clear', {body: {flowId, provider}}),

  getSuggestedPrompts: (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    request('/api/chat/suggested-prompts', {body: {hasBlock, hasFindings: hasFindings ?? false}}),

  getDemoRemaining: (): Promise<number> => request('/api/chat/demo-remaining', {method: 'GET'}),

  previewContext: (req: ChatRequest): Promise<ContextPreview> => request('/api/chat/preview-context', {body: req}),

  readSourceFiles: (files: string[]): Promise<Record<string, string>> =>
    request('/api/flow/read-sources', {body: {files}}),
}
