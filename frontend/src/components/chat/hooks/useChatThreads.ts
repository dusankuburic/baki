import {useCallback} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import type {FlowDocument, SourceFileInfo} from '@/types'

// Exchanges (user+assistant pairs) kept by Compact. Mirrored in the toolbar
// copy, so keep the two in step.
export const COMPACT_KEEP_PAIRS = 3

interface UseChatThreadsOptions {
  doc: FlowDocument | null
  activeThreadId: string | null
  sourceFiles: SourceFileInfo[]
}

export function useChatThreads({doc, activeThreadId, sourceFiles}: UseChatThreadsOptions) {
  const createThread = useChatStore(s => s.createThread)
  const closeThread = useChatStore(s => s.closeThread)
  const updateThread = useChatStore(s => s.updateThread)
  const clearThreadMessages = useChatStore(s => s.clearThreadMessages)
  const compactThread = useChatStore(s => s.compactThread)
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
  // The conversation key is (flowId, contextBlockId) — read it BEFORE the
  // update below resets the scope, or the wrong conversation gets cleared.
  const handleClearContext = useCallback(() => {
    if (!activeThreadId) return
    const thread = useChatStore.getState().threads.find(t => t.id === activeThreadId)
    clearThreadMessages(activeThreadId)
    updateThread(activeThreadId, {
      contextBlockId: null,
      selectedSourceFiles: sourceFiles.length > 0 ? sourceFiles.map(f => f.filename) : [],
      tokensIn: 0,
      tokensOut: 0,
    })
    if (doc) {
      chatApi.clearConversation(doc.id, thread?.contextBlockId || 'flow').catch(err => {
        logger.warn('Failed to clear conversation', err)
      })
    }
  }, [activeThreadId, clearThreadMessages, updateThread, sourceFiles, doc])

  // "Keep only the last N exchanges to reduce token usage". The saving only
  // happens if the BACKEND's copy shrinks too: buildRequest omits `messages`
  // and the server replays its own stored conversation, so trimming just the
  // local store left every prompt exactly as expensive as before — the one
  // thing this action exists to do.
  //
  // SaveConversation replaces the stored list wholesale, so writing the
  // trimmed messages back is the compaction.
  const handleCompact = useCallback(() => {
    if (!activeThreadId) return
    const thread = useChatStore.getState().threads.find(t => t.id === activeThreadId)
    compactThread(activeThreadId, COMPACT_KEEP_PAIRS)
    if (!doc) return
    const kept = useChatStore.getState().getMessages(activeThreadId)
    chatApi.saveConversation(doc.id, thread?.contextBlockId || 'flow', [...kept]).catch(err => {
      logger.warn('Failed to persist compacted conversation', err)
    })
  }, [activeThreadId, compactThread, doc])

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
