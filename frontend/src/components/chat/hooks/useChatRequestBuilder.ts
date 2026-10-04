import {useCallback} from 'react'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import type {ChatMessage, ChatRequest, FlowDocument, AISettings, ProviderID} from '@/types'

export interface BuildChatRequestArgs {
  // The thread as currently stored (selected files, tools toggle, scope).
  thread: ChatThread
  provider: ProviderID
  // The model the turn is for, as resolved per thread (lib/modelSelection):
  // sent as is. '' asks the server for its default — never the saved
  // "Default for new chats", which may name a model this scope no longer
  // offers (that is the resolver's call, not the request's).
  model: string
  aiSettings: AISettings
  // The thread's messages BEFORE this turn's user message.
  messages: readonly ChatMessage[]
  text: string
  overrideFiles?: string[]
  excludeContext?: boolean
  includeHistory?: boolean
  userMessageId?: string
  assistantMessageId?: string
}

// buildChatRequest assembles the chat request payload. It is pure so the
// always-mounted stream host can build a turn without the chat panel (a
// queued follow-up sends even while the panel is hidden), and so the request
// shape — "what goes into a turn" — can be tested on its own.
//
// It OMITS `messages` (history) by default: the backend reconstructs it from
// its conversation store, so the client no longer re-sends the full history
// each turn (~30KB saved/request). includeHistory=true is for a client that
// has locally truncated history (resend/edit) — the backend then uses the
// provided slice as-is instead of its stored copy. With "Save conversation
// history" off there IS no stored copy, so history always travels with the
// request.
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
  // The per-provider reasoning effort ('' = provider default), sent
  // next to temperature/maxTokens; the backend ignores it on models without
  // reasoning (one repair round trip at worst). Temperature, Max tokens and
  // the effort stay per provider; a registry entry's own max output caps the
  // request on the server.
  const reasoningEffort = providerConfig?.reasoningEffort ?? ''
  return {
    flowId: thread.flowId,
    provider,
    model,
    // Omit `messages` unless the caller needs to override server-side history.
    ...(sendHistory
      ? {
          messages: messages.map((m: ChatMessage) => ({
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
    conversationId: conversationKey(thread),
    contextBlockId: thread.contextBlockId || '',
    selectedSourceFiles: excludeContext ? undefined : filesToUse.length > 0 ? filesToUse : undefined,
    temperature: providerConfig?.temperature ?? 0.3,
    maxTokens: providerConfig?.maxTokens ?? 4096,
    ...(reasoningEffort ? {reasoningEffort} : {}),
    excludeContext: excludeContext ?? false,
    useTools: thread.useTools ?? false,
    ...(userMessageId ? {userMessageId} : {}),
    ...(assistantMessageId ? {assistantMessageId} : {}),
  }
}

interface UseChatRequestBuilderOptions {
  doc: FlowDocument | null
  activeThread: ChatThread | null
  // The active thread's provider/model (its resolved pair).
  provider: ProviderID
  selectedModel: string
  aiSettings: AISettings
  getMessages: (threadId: string) => readonly ChatMessage[]
}

// useChatRequestBuilder binds buildChatRequest to the active thread and its
// provider/model — used by the context preview, which shows exactly the
// request a send would make.
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
      // Read the thread from the store: the selected files / tools toggle may
      // have changed since this render captured activeThread.
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
