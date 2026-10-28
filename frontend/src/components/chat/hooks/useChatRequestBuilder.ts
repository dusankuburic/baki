import {useCallback} from 'react'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import {requestHistory} from '@/lib/conversationSync'
import type {ChatMessage, ChatRequest, FlowDocument, AISettings, ProviderID} from '@/types'

export interface BuildChatRequestArgs {
  thread: ChatThread
  provider: ProviderID
  model: string
  aiSettings: AISettings
  messages: readonly ChatMessage[]
  text: string
  overrideFiles?: string[]
  excludeContext?: boolean
  includeHistory?: boolean
  userMessageId?: string
  assistantMessageId?: string
}

/**
 * Whether the thread's turns run the grounding tool loop (R-frontend-6): the thread's own choice, else the AI
 * setting's default, which is on. On is safe for every model: the server falls back to the marker loop for one
 * without native tools. The header toggle and the request both read this.
 */
export function threadUsesTools(
  thread: Pick<ChatThread, 'useTools'> | null | undefined,
  aiSettings: Pick<AISettings, 'toolsOffByDefault'>,
): boolean {
  if (!thread) return false
  return thread.useTools ?? !aiSettings.toolsOffByDefault
}

export function buildChatRequest({
  thread,
  provider,
  model,
  aiSettings,
  messages,
  text,
  overrideFiles,
  excludeContext,
  includeHistory = false,
  userMessageId,
  assistantMessageId,
}: BuildChatRequestArgs): ChatRequest {
  const providerConfig = aiSettings.providers[provider as keyof typeof aiSettings.providers]
  let filesToUse = thread.selectedSourceFiles || []
  if (overrideFiles !== undefined && overrideFiles.length > 0) {
    filesToUse = overrideFiles
  }
  const sendHistory = includeHistory || aiSettings.saveConversationHistory === false
  const reasoningEffort = providerConfig?.reasoningEffort ?? ''
  return {
    flowId: thread.flowId,
    provider,
    model,
    ...(sendHistory ? {messages: requestHistory(messages)} : {}),
    userMessage: text,
    conversationId: conversationKey(thread),
    contextBlockId: thread.contextBlockId || '',
    ...(thread.contextBlockId && thread.contextBlockAnchor ? {contextBlockAnchor: thread.contextBlockAnchor} : {}),
    selectedSourceFiles: excludeContext ? undefined : filesToUse.length > 0 ? filesToUse : undefined,
    temperature: providerConfig?.temperature ?? 0.3,
    maxTokens: providerConfig?.maxTokens ?? 4096,
    ...(reasoningEffort ? {reasoningEffort} : {}),
    excludeContext: excludeContext ?? false,
    useTools: threadUsesTools(thread, aiSettings),
    ...(userMessageId ? {userMessageId} : {}),
    ...(assistantMessageId ? {assistantMessageId} : {}),
  }
}

interface UseChatRequestBuilderOptions {
  doc: FlowDocument | null
  activeThread: ChatThread | null
  provider: ProviderID
  selectedModel: string
  aiSettings: AISettings
  getMessages: (threadId: string) => readonly ChatMessage[]
}

export function useChatRequestBuilder({
  doc,
  activeThread,
  provider,
  selectedModel,
  aiSettings,
  getMessages,
}: UseChatRequestBuilderOptions) {
  const buildRequest = useCallback(
    (text: string, overrideFiles?: string[], excludeContext?: boolean, includeHistory = false) => {
      if (!doc || !activeThread) return null
      const currentThread = useChatStore.getState().threads.find(t => t.id === activeThread.id) ?? activeThread
      return buildChatRequest({
        thread: {...currentThread, flowId: doc.id},
        provider,
        model: selectedModel,
        aiSettings,
        messages: getMessages(activeThread.id),
        text,
        overrideFiles,
        excludeContext,
        includeHistory,
      })
    },
    [doc, activeThread, provider, selectedModel, aiSettings, getMessages],
  )

  return {buildRequest}
}
