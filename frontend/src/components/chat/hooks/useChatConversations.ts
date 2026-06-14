import {useState, useEffect} from 'react'
import {useChatStore, type ChatThread} from '@/stores/chatStore'
import {chatApi, flowApi} from '@/api'
import {logger} from '@/lib/logger'
import type {ChatMessage, SourceFileInfo, ConversationFile, FlowDocument} from '@/types'

const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

interface UseChatConversationsOptions {
  doc: FlowDocument | null
  flowThreads: ChatThread[]
  activeThreadId: string | null
}

export function useChatConversations({doc, flowThreads, activeThreadId}: UseChatConversationsOptions) {
  const [sourceFiles, setSourceFiles] = useState<SourceFileInfo[]>(EMPTY_SOURCE_FILES)

  const appendMessage = useChatStore(s => s.appendMessage)
  const createThread = useChatStore(s => s.createThread)
  const updateThread = useChatStore(s => s.updateThread)

  useEffect(() => {
    if (!doc) {
      setSourceFiles(EMPTY_SOURCE_FILES)
      return
    }
    let cancelled = false
    flowApi.getSourceFiles().then((files: SourceFileInfo[] | null) => {
      if (cancelled) return
      const list: SourceFileInfo[] = (files || []).map((f: SourceFileInfo) => ({
        filename: f.filename || '',
        subflowId: f.subflowId || '',
        subflowName: f.subflowName || '',
        blockCount: f.blockCount || 0,
        lineCount: f.lineCount || 0,
      }))
      setSourceFiles(list)
    }).catch((err) => { if (!cancelled) logger.warn('Failed to load source files', err) })
    return () => { cancelled = true }
  }, [doc])

  useEffect(() => {
    if (!doc) return
    if (flowThreads.length === 0) {
      const id = createThread(doc.id)
      if (sourceFiles.length > 0) {
        updateThread(id, {selectedSourceFiles: sourceFiles.map(f => f.filename)})
      }
      let cancelled = false
      chatApi.getConversation(doc.id, 'flow').then((conv: ConversationFile) => {
        if (cancelled) return
        if (conv?.messages?.length > 0) {
          for (const m of conv.messages) {
            appendMessage(id, m as ChatMessage)
          }
        }
      }).catch((err) => { if (!cancelled) logger.warn('Failed to load flow conversation', err) })
      return () => { cancelled = true }
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [doc?.id])

  useEffect(() => {
    if (!activeThreadId || !doc?.id) return
    const existing = useChatStore.getState().conversations.get(activeThreadId)
    if (existing !== undefined) return
    const thread = useChatStore.getState().threads.find(t => t.id === activeThreadId)
    const scope = thread?.contextBlockId || 'flow'
    let cancelled = false
    chatApi.getConversation(doc.id, scope).then((conv: ConversationFile) => {
      if (cancelled) return
      if (conv?.messages) {
        for (const m of conv.messages) {
          appendMessage(activeThreadId, m as ChatMessage)
        }
      }
    }).catch((err) => { logger.warn('Failed to load thread conversation', err) })
    return () => { cancelled = true }
  }, [activeThreadId, appendMessage, doc?.id])

  return {sourceFiles}
}
