import {useTranslation} from 'react-i18next'
import {useCallback, useEffect, useRef} from 'react'
import {chatApi} from '@/api'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import {mayHoldStoredConversation} from '@/stores/chat/history'
import {
  clearThreadConversation,
  replaceThreadConversation,
  isPersistable,
  isConversationUnowned,
} from '@/lib/conversationSync'
import {onThreadRekeyed, rekeyThread, settleThreadTitle} from '@/lib/threadIndex'
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

/**
 * Whether the server may hold a conversation for the thread: never with history saving off, and not for a thread
 * whose loaded history is empty (a new thread, or one cleared). One whose history has not loaded may.
 */
export function mayHaveStoredHistory(threadId: string): boolean {
  return conversationHistoryEnabled() && mayHoldStoredConversation(useChatStore.getState(), threadId)
}

function isNotFound(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as {status?: unknown}).status === 404
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
  const setContextBlock = useChatStore(s => s.setContextBlock)
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

  // A closed thread can never be reopened, so what the server stored for it goes too (R-frontend-c26): on the
  // version this client holds, so a conversation another tab or session moved on keeps the thread open instead.
  // A thread with a live stream was just stopped: its own server write moves the version, so it clears whole.
  // A thread whose key held another user's conversation moved to a new one (R-security-3): say so, once.
  useEffect(
    () =>
      onThreadRekeyed((_old, _new, reason) => {
        if (reason === 'keyTaken') toast?.info(t('errors.threadRekeyed'))
      }),
    [toast, t],
  )

  // A close whose clear is still in flight: a repeat of it (a second click) is ignored.
  const closingRef = useRef(new Set<string>())
  const handleCloseThread = useCallback(
    async (threadId: string) => {
      const st = useChatStore.getState()
      const thread = st.threads.find(th => th.id === threadId)
      if (!thread) return
      if (closingRef.current.has(threadId)) return
      closingRef.current.add(threadId)
      // A row may exist without any history: a name the server index stored makes one (a name not sent yet is
      // dropped, one on its way is waited for), so a renamed thread does not come back with the next sync.
      const named = settleThreadTitle(threadId)
      const rowMayExist = (typeof named === 'boolean' ? named : await named) || mayHaveStoredHistory(threadId)
      if (!rowMayExist) {
        closingRef.current.delete(threadId)
        closeThread(threadId)
        return
      }
      const key = conversationKey(thread)
      try {
        if (st.streams[threadId]) {
          await chatApi.clearConversation(thread.flowId, key)
        } else if (!(await clearThreadConversation(threadId, thread.flowId, key))) {
          toast?.warning(t('errors.changedElsewhere'))
          return
        }
      } catch (err) {
        // Best effort: nothing stored (404), or a clear this caller may not make; the thread goes either way. A
        // conversation stored before owners stays on the server (it goes with its flow): the user is told.
        if (isConversationUnowned(err)) toast?.info(t('errors.threadUnowned'))
        else if (!isNotFound(err)) logger.warn('Failed to delete the closed thread’s conversation', err)
      } finally {
        closingRef.current.delete(threadId)
      }
      closeThread(threadId)
    },
    [closeThread, toast, t],
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
        // The block scope goes as a whole: a kept anchor would be stale, and a kept lost-block notice would
        // outlive the conversation it was about.
        updateThread(threadId, {
          contextBlockId: null,
          contextBlockAnchor: null,
          contextBlockLost: undefined,
          selectedSourceFiles: sourceFiles.length > 0 ? sourceFiles.map(f => f.filename) : [],
          tokensIn: 0,
          tokensOut: 0,
        })
      })
      .catch(err => {
        if (isConversationUnowned(err)) {
          // Stored before owners, and not this user's to delete: the user asked for an empty thread, so the thread
          // starts afresh under a new key, and the old conversation stays on the server.
          const fresh = rekeyThread(threadId, 'unowned')
          if (fresh) {
            clearThreadMessages(fresh)
            updateThread(fresh, {
              contextBlockId: null,
              contextBlockAnchor: null,
              contextBlockLost: undefined,
              selectedSourceFiles: sourceFiles.length > 0 ? sourceFiles.map(f => f.filename) : [],
              tokensIn: 0,
              tokensOut: 0,
            })
          }
          toast?.info(t('errors.threadUnownedCleared'))
          return
        }
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
    const persistable = st.getMessages(threadId).filter(isPersistable)
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
        if (!stored) {
          toast?.warning(t('errors.changedElsewhere'))
          return
        }
        // Stored under a new key when the old one was another user's: the moved thread holds the list already.
        if (useChatStore.getState().threads.some(th => th.id === threadId)) setThreadMessages(threadId, kept)
      })
      .catch(err => {
        logger.warn('Failed to persist compacted conversation', err)
        toast?.error(t('errors.compactFailed'))
      })
  }, [activeThreadId, setThreadMessages, toast, t])

  const setThreadContextBlock = useCallback(
    (blockId: string | null) => {
      if (activeThreadId) setContextBlock(activeThreadId, blockId, doc)
    },
    [activeThreadId, setContextBlock, doc],
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
