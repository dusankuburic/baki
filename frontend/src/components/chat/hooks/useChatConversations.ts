import {useState, useEffect, useRef} from 'react'
import {useChatStore, type ChatThread} from '@/stores/chatStore'
import {chatApi, flowApi} from '@/api'
import {logger} from '@/lib/logger'
import {parseChatMessages} from '@/lib/chatMessage'
import type {SourceFileInfo, ConversationFile, FlowDocument} from '@/types'

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

  // sourceFilesRef mirrors sourceFiles so the thread-creation effect (which
  // depends only on [doc?.id]) reads the CURRENT file list instead of the stale
  // closure captured at mount — without this, an auto-created thread never
  // received the asynchronously-loaded source files (a manual create did,
  // because handleCreateThread reads live state).
  const sourceFilesRef = useRef<SourceFileInfo[]>(EMPTY_SOURCE_FILES)
  useEffect(() => {
    sourceFilesRef.current = sourceFiles
  }, [sourceFiles])

  // createdForDocRef guards the auto-create effect against React 18 StrictMode
  // double-mount (mount → cleanup → remount fires the effect twice with the
  // same empty flowThreads closure, creating two threads). The ref persists
  // across the StrictMode remount, so the second invocation sees the doc was
  // already handled and skips. Resets naturally on a doc change.
  const createdForDocRef = useRef<string | null>(null)

  useEffect(() => {
    if (!doc) {
      setSourceFiles(EMPTY_SOURCE_FILES)
      return
    }
    let cancelled = false
    flowApi
      .getSourceFiles()
      .then((files: SourceFileInfo[] | null) => {
        if (cancelled) return
        const list: SourceFileInfo[] = (files || []).map((f: SourceFileInfo) => ({
          filename: f.filename || '',
          subflowId: f.subflowId || '',
          subflowName: f.subflowName || '',
          blockCount: f.blockCount || 0,
          lineCount: f.lineCount || 0,
        }))
        setSourceFiles(list)
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load source files', err)
      })
    return () => {
      cancelled = true
    }
  }, [doc])

  useEffect(() => {
    if (!doc) return
    // StrictMode-safe: only auto-create once per doc. Without this, the
    // synchronous createThread fires twice (StrictMode remount) before the
    // store updates, producing two empty threads.
    if (createdForDocRef.current === doc.id) return
    if (flowThreads.length > 0) {
      createdForDocRef.current = doc.id
      return
    }
    createdForDocRef.current = doc.id
    const id = createThread(doc.id)
    const files = sourceFilesRef.current
    if (files.length > 0) {
      updateThread(id, {selectedSourceFiles: files.map(f => f.filename)})
    }
    let cancelled = false
    chatApi
      .getConversation(doc.id, 'flow')
      .then((conv: ConversationFile) => {
        if (cancelled) return
        // Validate the backend payload at the boundary instead of trusting the
        // shape (F5): malformed entries are dropped before reaching the store.
        for (const m of parseChatMessages(conv?.messages)) {
          appendMessage(id, m)
        }
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load flow conversation', err)
      })
    return () => {
      cancelled = true
    }
  }, [doc?.id, doc, flowThreads.length, createThread, updateThread, appendMessage])

  useEffect(() => {
    if (!activeThreadId || !doc?.id) return
    const existing = useChatStore.getState().conversations.get(activeThreadId)
    if (existing !== undefined) return
    const thread = useChatStore.getState().threads.find(t => t.id === activeThreadId)
    const scope = thread?.contextBlockId || 'flow'
    let cancelled = false
    chatApi
      .getConversation(doc.id, scope)
      .then((conv: ConversationFile) => {
        if (cancelled) return
        for (const m of parseChatMessages(conv?.messages)) {
          appendMessage(activeThreadId, m)
        }
      })
      .catch(err => {
        logger.warn('Failed to load thread conversation', err)
      })
    return () => {
      cancelled = true
    }
  }, [activeThreadId, appendMessage, doc?.id])

  return {sourceFiles}
}
