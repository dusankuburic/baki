import {useState, useEffect, useRef, useCallback} from 'react'
import {useChatStore, conversationKey, LEGACY_FLOW_SCOPE, type ChatThread} from '@/stores/chatStore'
import {chatApi, flowApi} from '@/api'
import {logger} from '@/lib/logger'
import type {SourceFileInfo, FlowDocument} from '@/types'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import {isTauri} from '@/platform/guards'

const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

interface UseChatConversationsOptions {
  doc: FlowDocument | null
  flowThreads: ChatThread[]
  activeThreadId: string | null
  /**
   * Re-attach a stream the conversation load reports as still in-flight
   * (F-21) — the chat stream engine's resumeAfterReload. Optional so tests
   * and non-panel hosts can mount the hook without the engine.
   */
  resumeActiveStream?: (threadId: string, streamId: string, onGone: () => void) => boolean
}

// loadThreadHistory fetches a thread's server-side conversation under a
// generation token (see chatStore.beginHistoryLoad): a result that lands after
// the thread was cleared, reloaded or closed is dropped, and loaded messages
// are merged BEFORE anything the user sent meanwhile. A failed load leaves the
// thread in 'error', which blocks every full-replace save of it — the server
// copy is never overwritten with just the turns on screen. AITab shows the
// failure inline, with Retry.
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
      // A stale load (the thread was cleared, reloaded or closed meanwhile)
      // neither applies nor re-attaches: a newer load owns the thread.
      if (!useChatStore.getState().finishHistoryLoad(threadId, token, load.messages)) return
      // A turn recorded as in flight (chatStore.InFlightTurn) that the
      // server no longer reports ended while this page was away: its record
      // goes, as it does when a turn ends here.
      const now = useChatStore.getState()
      const inFlight = now.threads.find(t => t.id === threadId)?.inFlight
      if (inFlight && !now.streams[threadId] && inFlight.streamId !== load.activeStreamId) {
        now.updateThread(threadId, {inFlight: undefined})
      }
      // F-21: the server says a stream for this conversation is still
      // in-flight (a reload / reopen happened mid-turn): re-attach to it
      // instead of showing the thread without its answer. The user turn is
      // already in `messages` — the server persisted it at stream start. If
      // the stream turns out to be gone, the engine drops it and refetches
      // here, where the server's persisted reply (if any) lands. That refetch
      // does not re-attach again (one shot): the engine reports any resume
      // failure as gone, and a server still listing the stream would
      // otherwise loop GET → SSE → resume without limit.
      if (load.activeStreamId && !useChatStore.getState().streams[threadId]) {
        resumeActiveStream?.(threadId, load.activeStreamId, () => loadThreadHistory(threadId))
      }
    })
    .catch(err => {
      logger.warn('Failed to load thread conversation', err)
      useChatStore.getState().finishHistoryLoad(threadId, token, null)
    })
}

// importLegacyConversation restores a flow's pre-thread history (stored under
// the flow-wide LEGACY_FLOW_SCOPE key) into the thread just created for it,
// then mirrors it under the thread's own key — the key the server reconstructs
// history from on the next turn.
//
// Desktop only. In the web app conversations are not scoped per user on the
// server and the legacy key is predictable, so importing it would hand one
// user a collaborator's conversation.
function importLegacyConversation(flowId: string, threadId: string): void {
  const token = useChatStore.getState().beginHistoryLoad(threadId)
  chatApi
    .getConversation(flowId, LEGACY_FLOW_SCOPE)
    .then(load => {
      const messages = load.messages
      if (!useChatStore.getState().finishHistoryLoad(threadId, token, messages)) return
      // A turn already in flight saves the merged list itself on done.
      if (messages.length === 0 || !conversationHistoryEnabled() || useChatStore.getState().streams[threadId]) return
      const merged = useChatStore.getState().getMessages(threadId)
      // Mirrored, then the legacy copy is cleared: it is imported once, not
      // again into every thread created after the flow's last one is closed.
      chatApi
        .saveConversation(flowId, conversationKey({id: threadId}), [...merged])
        .then(() => chatApi.clearConversation(flowId, LEGACY_FLOW_SCOPE))
        .catch(err => {
          logger.warn('Failed to migrate legacy conversation to thread key', err)
        })
    })
    .catch(err => {
      // Nothing to import is the common case (and not worth a toast): the
      // thread just stays empty, and saves stay allowed.
      logger.warn('Failed to load flow conversation', err)
      if (useChatStore.getState().historyStatus[threadId]?.token === token) {
        useChatStore.getState().markHistoryLoaded(threadId)
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
  // Read through a ref so a host's inline callback cannot change the history
  // effect's deps and re-fetch the conversation on every render.
  const resumeActiveStreamRef = useRef(resumeActiveStream)
  useEffect(() => {
    resumeActiveStreamRef.current = resumeActiveStream
  })

  const createThread = useChatStore(s => s.createThread)
  const hydrateThreads = useChatStore(s => s.hydrateThreads)
  const updateThread = useChatStore(s => s.updateThread)
  const historyState = useChatStore(s => (activeThreadId ? s.historyStatus[activeThreadId]?.state : undefined))

  const docId = doc?.id ?? null

  // Source files are a local-filesystem (desktop) feature — the endpoint
  // answers 403 in cloud mode by design, so it is only called in Tauri.
  //
  // sourcesForRef records which flow the current list belongs to, and
  // pendingFilesThreadRef the thread auto-created before its flow's list
  // arrived: that thread receives the list when the fetch resolves. Reading a
  // ref at creation time instead handed the new thread an empty list (first
  // flow) or the PREVIOUS flow's filenames (after a switch).
  const sourcesForRef = useRef<string | null>(null)
  const pendingFilesThreadRef = useRef<string | null>(null)
  const sourceFilesRef = useRef<SourceFileInfo[]>(EMPTY_SOURCE_FILES)
  useEffect(() => {
    sourceFilesRef.current = sourceFiles
  }, [sourceFiles])

  useEffect(() => {
    if (!doc || !isTauri()) {
      sourcesForRef.current = null
      // Clears the list when there is no document / no local filesystem.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setSourceFiles(EMPTY_SOURCE_FILES)
      return
    }
    // A different flow: drop the previous flow's list right away rather than
    // show its files until the new fetch lands. A same-flow refresh (reimport)
    // keeps the list while it refetches.
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

  // The open flow always has a thread to type into: one is created when none
  // is in memory and none is stored — the first open, and again after the
  // user closes the flow's last thread (or the flow is cleared). The guard
  // reads the STORE, not a "done for this doc" ref: React 18 StrictMode's
  // double-invoke then creates one thread (the second run sees the first's)
  // without also suppressing the re-create.
  useEffect(() => {
    if (!docId) return
    if (useChatStore.getState().threads.some(th => th.flowId === docId)) return
    // Persisted threads come back with their ids, so their conversations are
    // reachable under the same keys; the active-thread effect below loads them.
    if (hydrateThreads(docId).length > 0) return
    const id = createThread(docId)
    const files = sourceFilesRef.current
    if (sourcesForRef.current === docId && files.length > 0) {
      updateThread(id, {selectedSourceFiles: files.map(f => f.filename)})
    } else if (isTauri()) {
      pendingFilesThreadRef.current = id
    }
    // No persisted threads: this flow's pre-thread history (if any) still
    // lives under the legacy flow-wide key.
    if (isTauri()) importLegacyConversation(docId, id)
  }, [docId, flowThreads.length, createThread, hydrateThreads, updateThread])

  // Keep a thread selected for the open flow. setDocument clears the active
  // thread on every load — including a SAME-flow refresh (Findings "Fix",
  // History restore, toolbar refresh) and a switch back to a flow whose
  // threads are still in memory — and nothing re-selected one, so the chat
  // dropped to its empty state mid-conversation.
  useEffect(() => {
    if (!docId) return
    const st = useChatStore.getState()
    const active = st.threads.find(th => th.id === st.activeThreadId)
    if (active && active.flowId === docId) return
    const next = st.threadForFlow(docId)
    if (next) st.switchThread(next)
  }, [docId, activeThreadId, flowThreads.length])

  // Load the active thread's server history once per thread. A thread whose
  // last load failed is retried when it becomes active again.
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
