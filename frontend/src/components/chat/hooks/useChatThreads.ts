import {useTranslation} from 'react-i18next'
import {useCallback} from 'react'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {useOptionalToast} from '@/components/shared/Toast'
import type {ChatMessage, FlowDocument, SourceFileInfo} from '@/types'
import {conversationHistoryEnabled} from '@/lib/chatHistory'

// Exchanges (user+assistant pairs) kept by Compact. Mirrored in the toolbar
// copy, so keep the two in step.
export const COMPACT_KEEP_PAIRS = 3

// lastExchanges keeps the last `pairs` user turns and everything after each
// (their replies), cut ON a user message: a fixed count of messages could
// open the kept slice with an answer to a question that is gone.
export function lastExchanges(messages: readonly ChatMessage[], pairs: number): ChatMessage[] {
  let seen = 0
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === 'user' && ++seen === pairs) return messages.slice(i)
  }
  return [...messages]
}

interface UseChatThreadsOptions {
  doc: FlowDocument | null
  activeThreadId: string | null
  sourceFiles: SourceFileInfo[]
}

export function useChatThreads({doc, activeThreadId, sourceFiles}: UseChatThreadsOptions) {
  const {t} = useTranslation('chat')
  const toast = useOptionalToast()
  const createThread = useChatStore(s => s.createThread)
  const closeThread = useChatStore(s => s.closeThread)
  const updateThread = useChatStore(s => s.updateThread)
  const clearThreadMessages = useChatStore(s => s.clearThreadMessages)
  const setThreadMessages = useChatStore(s => s.setThreadMessages)
  const switchThread = useChatStore(s => s.switchThread)

  const handleCreateThread = useCallback(() => {
    if (!doc) return
    const id = createThread(doc.id)
    if (sourceFiles.length > 0) {
      updateThread(id, {selectedSourceFiles: sourceFiles.map(f => f.filename)})
    }
  }, [doc, createThread, sourceFiles, updateThread])

  const handleCloseThread = useCallback(
    (threadId: string) => {
      closeThread(threadId)
    },
    [closeThread],
  )

  const handleRenameThread = useCallback(
    (threadId: string, title: string) => {
      updateThread(threadId, {title})
    },
    [updateThread],
  )

  // "Delete all messages in this thread" — the label promises a real clear, so
  // it must reach the BACKEND too. buildRequest omits `messages` by default and
  // the server replays its own stored conversation, so wiping only the local
  // store left the model still seeing every prior turn: the panel looked empty
  // while the next answer referred back to a conversation the user thought they
  // had deleted. (/clear, via handleClearThread, always did this correctly.)
  //
  // The server copy is cleared FIRST and the panel only after it succeeded:
  // clearing locally while the request failed showed an empty thread whose
  // "deleted" turns the model kept answering from.
  const handleClearContext = useCallback(() => {
    if (!activeThreadId) return
    const threadId = activeThreadId
    const thread = useChatStore.getState().threads.find(th => th.id === threadId)
    if (!thread) return
    chatApi
      .clearConversation(thread.flowId, conversationKey(thread))
      .then(() => {
        // Re-checked on return: a turn sent meanwhile must not be wiped, and a
        // thread closed meanwhile must not come back.
        const st = useChatStore.getState()
        if (!st.threads.some(th => th.id === threadId) || st.streams[threadId]) return
        clearThreadMessages(threadId)
        updateThread(threadId, {
          contextBlockId: null,
          selectedSourceFiles: sourceFiles.length > 0 ? sourceFiles.map(f => f.filename) : [],
          tokensIn: 0,
          tokensOut: 0,
        })
      })
      .catch(err => {
        logger.warn('Failed to clear conversation', err)
        toast?.error(t('errors.clearFailed'))
      })
  }, [activeThreadId, clearThreadMessages, updateThread, sourceFiles, toast, t])

  // "Keep only the last N exchanges to reduce token usage". The saving only
  // happens if the BACKEND's copy shrinks too: buildRequest omits `messages`
  // and the server replays its own stored conversation, so trimming just the
  // local store left every prompt exactly as expensive as before — the one
  // thing this action exists to do.
  //
  // SaveConversation replaces the stored list wholesale, so writing the
  // trimmed messages back is the compaction — and the SAME list is then
  // applied locally, so screen and server hold identical histories (trimming
  // the two by different rules left them disagreeing whenever an error
  // bubble sat near the end).
  //
  // Only a thread whose server history has loaded can be compacted: the kept
  // slice would otherwise be cut from an incomplete local view and REPLACE
  // the stored conversation. Error bubbles are never saved (they are
  // transient), matching the engine's save-on-done.
  const handleCompact = useCallback(() => {
    if (!activeThreadId) return
    const threadId = activeThreadId
    const st = useChatStore.getState()
    const thread = st.threads.find(th => th.id === threadId)
    if (!thread) return
    const persistable = st.getMessages(threadId).filter(m => m.finishReason !== 'error')
    const kept = lastExchanges(persistable, COMPACT_KEEP_PAIRS)
    // History off: the server holds no copy to shrink, and the next request
    // carries the trimmed local messages itself.
    if (!conversationHistoryEnabled()) {
      setThreadMessages(threadId, kept)
      return
    }
    if (!st.isHistorySynced(threadId)) {
      toast?.warning(t('errors.historyNotReady'))
      return
    }
    chatApi
      .saveConversation(thread.flowId, conversationKey(thread), kept)
      .then(() => setThreadMessages(threadId, kept))
      .catch(err => {
        logger.warn('Failed to persist compacted conversation', err)
        toast?.error(t('errors.compactFailed'))
      })
  }, [activeThreadId, setThreadMessages, toast, t])

  const setThreadContextBlock = useCallback(
    (blockId: string | null) => {
      if (activeThreadId) updateThread(activeThreadId, {contextBlockId: blockId})
    },
    [activeThreadId, updateThread],
  )

  const setThreadSourceFiles = useCallback(
    (files: string[]) => {
      if (activeThreadId) updateThread(activeThreadId, {selectedSourceFiles: files})
    },
    [activeThreadId, updateThread],
  )

  const setThreadUseTools = useCallback(
    (useTools: boolean) => {
      if (activeThreadId) updateThread(activeThreadId, {useTools})
    },
    [activeThreadId, updateThread],
  )

  return {
    switchThread,
    handleCreateThread,
    handleCloseThread,
    handleRenameThread,
    handleClearContext,
    handleCompact,
    setThreadContextBlock,
    setThreadSourceFiles,
    setThreadUseTools,
  }
}
