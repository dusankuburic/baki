import {useCallback} from 'react'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import type {ChatMessage, FlowDocument, AISettings, ProviderID} from '@/types'

interface UseChatRequestBuilderOptions {
  doc: FlowDocument | null
  activeThread: ChatThread | null
  provider: ProviderID
  selectedModel: string
  aiSettings: AISettings
  getMessages: (threadId: string) => readonly ChatMessage[]
}

// useChatRequestBuilder assembles the chat request payload. Extracted from
// useAIChat so the request shape (and its "what goes into a turn" invariants)
// can be reasoned about and tested independently of the streaming machinery.
export function useChatRequestBuilder({
  doc,
  activeThread,
  provider,
  selectedModel,
  aiSettings,
  getMessages,
}: UseChatRequestBuilderOptions) {
  // buildRequest assembles the chat request. By default it OMITS `messages`
  // (history): the backend reconstructs it from its conversation store, so the
  // client no longer re-sends the full history each turn (~30KB saved/request).
  // Set includeHistory=true when the client has locally truncated history
  // (resend/edit) — the backend then uses the provided slice as-is instead of
  // its stored copy. With "Save conversation history" off there IS no stored
  // copy, so history always travels with the request.
  const buildRequest = useCallback(
    (text: string, overrideFiles?: string[], excludeContext?: boolean, includeHistory = false) => {
      if (!doc || !activeThread) return null
      const providerConfig = aiSettings.providers[provider as keyof typeof aiSettings.providers]
      const currentThread = useChatStore.getState().threads.find(t => t.id === activeThread.id)
      let filesToUse = currentThread?.selectedSourceFiles || []
      if (overrideFiles !== undefined && overrideFiles.length > 0) {
        filesToUse = overrideFiles
      }
      const sendHistory = includeHistory || aiSettings.saveConversationHistory === false
      return {
        flowId: doc.id,
        provider,
        model: selectedModel || providerConfig?.defaultModel || '',
        // Omit `messages` unless the caller needs to override server-side history.
        ...(sendHistory
          ? {
              messages: getMessages(activeThread.id).map((m: ChatMessage) => ({
                id: m.id,
                role: m.role,
                content: m.content,
                timestamp: m.timestamp,
              })),
            }
          : {}),
        userMessage: text,
        // conversationId is the server-side conversation-history key: one per
        // thread, so parallel threads on a flow never replay or overwrite each
        // other's history. contextBlockId still travels for context injection
        // (and as the legacy key for servers that predate conversationId).
        // excludeContext below gates only flow/block CONTEXT INJECTION.
        conversationId: conversationKey(activeThread),
        contextBlockId: activeThread.contextBlockId || '',
        selectedSourceFiles: excludeContext ? undefined : filesToUse.length > 0 ? filesToUse : undefined,
        temperature: providerConfig?.temperature ?? 0.3,
        maxTokens: providerConfig?.maxTokens ?? 4096,
        excludeContext: excludeContext ?? false,
        useTools: currentThread?.useTools ?? false,
      }
    },
    [doc, activeThread, provider, selectedModel, aiSettings, getMessages],
  )

  return {buildRequest}
}
