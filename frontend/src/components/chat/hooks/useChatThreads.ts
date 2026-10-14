import {useTranslation} from 'react-i18next'
import {useCallback} from 'react'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import {clearThreadConversation, replaceThreadConversation} from '@/lib/conversationSync'
import {logger} from '@/lib/logger'
import {useOptionalToast} from '@/components/shared/Toast'
import type {ChatMessage, FlowDocument, SourceFileInfo} from '@/types'
import {conversationHistoryEnabled} from '@/lib/chatHistory'

export const COMPACT_KEEP_PAIRS = 3

export function lastExchanges(messages: readonly ChatMessage[], pairs: number): ChatMessage[] {
  let seen = 0
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i]?.role === 'user' && ++seen === pairs) return messages.slice(i)
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

  const handleClearContext = useCallback(() => {
    if (!activeThreadId) return
    const threadId = activeThreadId
    const thread = useChatStore.getState().threads.find(th => th.id === threadId)
    if (!thread) return
    clearThreadConversation(threadId, thread.flowId, conversationKey(thread))
      .then(cleared => {
        if (!cleared) {
          toast?.warning(t('errors.changedElsewhere'))
          return
        }
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

  const handleCompact = useCallback(() => {
    if (!activeThreadId) return
    const threadId = activeThreadId
    const st = useChatStore.getState()
    const thread = st.threads.find(th => th.id === threadId)
    if (!thread) return
    const persistable = st.getMessages(threadId).filter(m => m.finishReason !== 'error')
    const kept = lastExchanges(persistable, COMPACT_KEEP_PAIRS)
    if (!conversationHistoryEnabled()) {
      setThreadMessages(threadId, kept)
      return
    }
    if (!st.isHistorySynced(threadId)) {
      toast?.warning(t('errors.historyNotReady'))
      return
    }
    replaceThreadConversation(threadId, thread.flowId, conversationKey(thread), kept)
      .then(stored => {
        if (stored) setThreadMessages(threadId, kept)
        else toast?.warning(t('errors.changedElsewhere'))
      })
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
