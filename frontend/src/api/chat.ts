import {request} from './client'
import {createAdapter} from '@/platform/adapters'
import type {ChatRequest, ChatMessage, ConversationFile, ContextPreview} from '@/types/domain'

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> =>
    request('/api/chat/stream', req),

  beginStream: (id: string): Promise<void> =>
    request('/api/chat/begin', {id}),

  cancelStream: (id: string): Promise<void> =>
    request('/api/chat/cancel', {id}),

  resumeStream: (id: string): Promise<{text: string; done: boolean; error: string; tokensIn: number; tokensOut: number}> =>
    request('/api/chat/resume', {id}),

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
