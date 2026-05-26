import {request} from './client'
import {save} from '@tauri-apps/plugin-dialog'
import type {ChatRequest, ChatMessage, ConversationFile, ContextPreview, SourceFileInfo} from '@/types/domain'

export const chatApi = {
  streamChatMessage: (req: ChatRequest): Promise<string> =>
    request('/api/chat/stream', req),

  beginStream: (streamId: string): Promise<void> =>
    request('/api/chat/begin', {streamId}),

  cancelStream: (streamId: string): Promise<void> =>
    request('/api/chat/cancel', {streamId}),

  getConversation: (flowId: string, provider: string): Promise<ConversationFile> =>
    request('/api/chat/get', {flowId, provider}),

  saveConversation: (flowId: string, provider: string, messages: ChatMessage[]): Promise<void> =>
    request('/api/chat/save', {flowId, provider, messages}),

  clearConversation: (flowId: string, provider: string): Promise<void> =>
    request('/api/chat/clear', {flowId, provider}),

  getSuggestedPrompts: (hasBlock: boolean, hasFindings?: boolean): Promise<string[]> =>
    request('/api/chat/suggested-prompts', {hasBlock, hasFindings: hasFindings ?? false}),

  getDemoRemaining: (): Promise<number> =>
    request('/api/chat/demo-remaining'),

  previewContext: (req: ChatRequest): Promise<ContextPreview> =>
    request('/api/chat/preview-context', req),

  exportConversation: async (flowId: string, provider: string): Promise<void> => {
    const path = await save({
      filters: [{name: 'Markdown', extensions: ['md']}]
    })
    if (!path) return
    await request('/api/chat/export', {flowId, provider, path})
  },

  getSourceFiles: (): Promise<SourceFileInfo[]> =>
    request('/api/flow/source-files'),

  readSourceFiles: (files: string[]): Promise<Record<string, string>> =>
    request('/api/flow/read-sources', {files}),
}
