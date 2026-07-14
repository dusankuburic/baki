import {useCallback} from 'react'
import {useChatStore} from '@/stores/chatStore'
import type {FlowDocument, SourceFileInfo} from '@/types'

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

  const handleClearContext = useCallback(() => {
    if (!activeThreadId) return
    clearThreadMessages(activeThreadId)
    updateThread(activeThreadId, {
      contextBlockId: null,
      selectedSourceFiles: sourceFiles.length > 0 ? sourceFiles.map(f => f.filename) : [],
      tokensIn: 0,
      tokensOut: 0,
    })
  }, [activeThreadId, clearThreadMessages, updateThread, sourceFiles])

  const handleCompact = useCallback(() => {
    if (!activeThreadId) return
    compactThread(activeThreadId, 3)
  }, [activeThreadId, compactThread])

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
