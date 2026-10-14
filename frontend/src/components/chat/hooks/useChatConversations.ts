import {useState, useEffect, useRef, useCallback} from 'react'
import {useChatStore, conversationKey, LEGACY_FLOW_SCOPE, type ChatThread} from '@/stores/chatStore'
import {chatApi, flowApi} from '@/api'
import {logger} from '@/lib/logger'
import type {SourceFileInfo, FlowDocument} from '@/types'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import {saveThreadConversation} from '@/lib/conversationSync'
import {isTauri} from '@/platform/guards'

const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

interface UseChatConversationsOptions {
  doc: FlowDocument | null
  flowThreads: ChatThread[]
  activeThreadId: string | null
  resumeActiveStream?: (threadId: string, streamId: string, onGone: () => void) => boolean
}

function loadThreadHistory(
  threadId: string,
  resumeActiveStream?: (threadId: string, streamId: string, onGone: () => void) => boolean,
): void {
  const st = useChatStore.getState()
  const thread = st.threads.find(t => t.id === threadId)
  if (!thread) return
  const token = st.beginHistoryLoad(threadId)
  chatApi
    .getConversation(thread.flowId, conversationKey(thread))
    .then(load => {
      if (!useChatStore.getState().finishHistoryLoad(threadId, token, load.messages, load.version)) return
      const now = useChatStore.getState()
      const inFlight = now.threads.find(t => t.id === threadId)?.inFlight
      if (inFlight && !now.streams[threadId] && inFlight.streamId !== load.activeStreamId) {
        now.updateThread(threadId, {inFlight: undefined})
      }
      if (load.activeStreamId && !useChatStore.getState().streams[threadId]) {
        resumeActiveStream?.(threadId, load.activeStreamId, () => loadThreadHistory(threadId))
      }
    })
    .catch(err => {
      logger.warn('Failed to load thread conversation', err)
      useChatStore.getState().finishHistoryLoad(threadId, token, null)
    })
}

function importLegacyConversation(flowId: string, threadId: string): void {
  // What is read here is the flow's legacy conversation; the version belongs to the thread's own key, which the
  // load does not touch, so the thread keeps the one it was created with.
  const held = useChatStore.getState().getConversationVersion(threadId)
  const token = useChatStore.getState().beginHistoryLoad(threadId)
  chatApi
    .getConversation(flowId, LEGACY_FLOW_SCOPE)
    .then(load => {
      const messages = load.messages
      if (!useChatStore.getState().finishHistoryLoad(threadId, token, messages, held)) return
      if (messages.length === 0 || !conversationHistoryEnabled() || useChatStore.getState().streams[threadId]) return
      // Saves on the thread's version and adopts the one the server answers.
      saveThreadConversation(threadId, flowId, conversationKey({id: threadId}))
        .then(() => chatApi.clearConversation(flowId, LEGACY_FLOW_SCOPE))
        .catch(err => {
          logger.warn('Failed to migrate legacy conversation to thread key', err)
        })
    })
    .catch(err => {
      logger.warn('Failed to load flow conversation', err)
      if (useChatStore.getState().historyStatus[threadId]?.token === token) {
        useChatStore.getState().markHistoryLoaded(threadId)
        if (held !== undefined) useChatStore.getState().setConversationVersion(threadId, held)
      }
    })
}

export function useChatConversations({
  doc,
  flowThreads,
  activeThreadId,
  resumeActiveStream,
}: UseChatConversationsOptions) {
  const [sourceFiles, setSourceFiles] = useState<SourceFileInfo[]>(EMPTY_SOURCE_FILES)
  const resumeActiveStreamRef = useRef(resumeActiveStream)
  useEffect(() => {
    resumeActiveStreamRef.current = resumeActiveStream
  })

  const createThread = useChatStore(s => s.createThread)
  const hydrateThreads = useChatStore(s => s.hydrateThreads)
  const updateThread = useChatStore(s => s.updateThread)
  const historyState = useChatStore(s => (activeThreadId ? s.historyStatus[activeThreadId]?.state : undefined))

  const docId = doc?.id ?? null

  const sourcesForRef = useRef<string | null>(null)
  const pendingFilesThreadRef = useRef<string | null>(null)
  const sourceFilesRef = useRef<SourceFileInfo[]>(EMPTY_SOURCE_FILES)
  useEffect(() => {
    sourceFilesRef.current = sourceFiles
  }, [sourceFiles])

  useEffect(() => {
    if (!doc || !isTauri()) {
      sourcesForRef.current = null
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setSourceFiles(EMPTY_SOURCE_FILES)
      return
    }
    if (sourcesForRef.current !== doc.id) {
      sourcesForRef.current = null
      setSourceFiles(EMPTY_SOURCE_FILES)
    }
    const flowId = doc.id
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
        sourcesForRef.current = flowId
        setSourceFiles(list)
        const pending = pendingFilesThreadRef.current
        pendingFilesThreadRef.current = null
        if (!pending || list.length === 0) return
        const thread = useChatStore.getState().threads.find(th => th.id === pending)
        if (thread && thread.flowId === flowId && thread.selectedSourceFiles.length === 0) {
          updateThread(pending, {selectedSourceFiles: list.map(f => f.filename)})
        }
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load source files', err)
      })
    return () => {
      cancelled = true
    }
  }, [doc, updateThread])

  useEffect(() => {
    if (!docId) return
    if (useChatStore.getState().threads.some(th => th.flowId === docId)) return
    if (hydrateThreads(docId).length > 0) return
    const id = createThread(docId)
    const files = sourceFilesRef.current
    if (sourcesForRef.current === docId && files.length > 0) {
      updateThread(id, {selectedSourceFiles: files.map(f => f.filename)})
    } else if (isTauri()) {
      pendingFilesThreadRef.current = id
    }
    if (isTauri()) importLegacyConversation(docId, id)
  }, [docId, flowThreads.length, createThread, hydrateThreads, updateThread])

  useEffect(() => {
    if (!docId) return
    const st = useChatStore.getState()
    const active = st.threads.find(th => th.id === st.activeThreadId)
    if (active && active.flowId === docId) return
    const next = st.threadForFlow(docId)
    if (next) st.switchThread(next)
  }, [docId, activeThreadId, flowThreads.length])

  useEffect(() => {
    if (!activeThreadId || !docId) return
    const status = useChatStore.getState().historyStatus[activeThreadId]
    if (status && status.state !== 'error') return
    loadThreadHistory(activeThreadId, resumeActiveStreamRef.current)
  }, [activeThreadId, docId])

  const retryHistoryLoad = useCallback(() => {
    if (activeThreadId) loadThreadHistory(activeThreadId, resumeActiveStreamRef.current)
  }, [activeThreadId])

  return {sourceFiles, historyState, retryHistoryLoad}
}
