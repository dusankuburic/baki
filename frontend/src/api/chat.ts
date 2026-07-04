import {request} from './client'
import {createAdapter} from '@/platform/adapters'
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
  streamChatMessage: (req: ChatRequest): Promise<string> =>
    request('/api/chat/stream', req),

  beginStream: (id: string): Promise<BeginStreamResult> =>
    request('/api/chat/begin', {id}),

  cancelStream: (id: string): Promise<void> =>
    request('/api/chat/cancel', {id}),

  resumeStream: (id: string, from = 0): Promise<{text: string; done: boolean; error: string; tokensIn: number; tokensOut: number}> =>
    request('/api/chat/resume', {id, from}),

  getConversation: (flowId: string, provider: string): Promise<ConversationFile> =>
    request('/api/chat/get', {flowId, provider}),

  saveConversation: (flowId: string, provider: string, messages: ChatMessage[]): Promise<void> =>
    request('/api/chat/save', {flowId, provider, messages}),

  clearConversation: (flowId: string, provider: string): Promise<void> =>
    request('/api/chat/clear', {flowId, provider}),

  getSuggestedPrompts: (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    request('/api/chat/suggested-prompts', {hasBlock, hasFindings: hasFindings ?? false}),

  getDemoRemaining: (): Promise<number> =>
    request('/api/chat/demo-remaining', undefined, 'GET'),

  previewContext: (req: ChatRequest): Promise<ContextPreview> =>
    request('/api/chat/preview-context', req),

  exportConversation: async (flowId: string, provider: string): Promise<void> => {
    const path = await createAdapter().fileSave({
      filters: [{name: 'Markdown', extensions: ['md']}]
    })
    if (!path) return
    await request('/api/chat/export', {flowId, provider, path})
  },

  readSourceFiles: (files: string[]): Promise<Record<string, string>> =>
    request('/api/flow/read-sources', {files}),
}
