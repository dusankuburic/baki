import {useState, useCallback, useMemo, useRef, useEffect} from 'react'
import {useChatStore, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi, ApiError} from '@/api'
import {useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import {conversationToMarkdown, downloadTextFile, safeFilename} from '@/lib/chatExport'
import {useChatConversations} from './useChatConversations'
import {useChatThreads} from './useChatThreads'
import {useChatRequestBuilder} from './useChatRequestBuilder'
import {useChatStreamEngine} from './useChatStreamEngine'
import type {ChatMessage, ContextPreview, ChatRequest} from '@/types'

interface UseAIChatOptions {
  selectedModel: string
}

const EMPTY_ARRAY: ChatMessage[] = []

export function useAIChat({selectedModel}: UseAIChatOptions) {
  const doc = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)

  const provider = useChatStore(s => s.selectedProvider)
  const aiSettings = useSettingsStore(s => s.settings.ai)

  const getMessages = useChatStore(s => s.getMessages)
  const appendMessage = useChatStore(s => s.appendMessage)
  const removeMessage = useChatStore(s => s.removeMessage)
  const clearThreadMessages = useChatStore(s => s.clearThreadMessages)
  const updateThread = useChatStore(s => s.updateThread)
  const startStream = useChatStore(s => s.startStream)
  const endStream = useChatStore(s => s.endStream)

  const threads = useChatStore(s => s.threads)
  const activeThreadId = useChatStore(s => s.activeThreadId)
  const activeThreadMessages = useChatStore(s =>
    s.activeThreadId ? (s.conversations.get(s.activeThreadId) ?? EMPTY_ARRAY) : EMPTY_ARRAY,
  )

  const flowThreads = useMemo(() => (doc ? threads.filter(t => t.flowId === doc.id) : []), [doc, threads])
  const activeThread = useMemo(() => threads.find(t => t.id === activeThreadId) ?? null, [threads, activeThreadId])

  const {sourceFiles} = useChatConversations({doc, flowThreads, activeThreadId})
  const threadActions = useChatThreads({doc, activeThreadId, sourceFiles})

  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingMessage, setPendingMessage] = useState<string | null>(null)
  const [pendingExcludeContext, setPendingExcludeContext] = useState<boolean | undefined>(undefined)

  // Streaming state is per-thread (chatStore.streams); this hook surfaces the
  // ACTIVE thread's slot. Narrow selectors keep per-chunk re-renders scoped:
  // only slot fields used here trigger updates.
  const isStreaming = useChatStore(s => Object.keys(s.streams).length > 0)
  const isCurrentThreadStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const showThinking = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]?.isThinking))
  const toolStatus = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolStatus ?? null) : null))
  const fixProposals = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.fixProposals ?? []) : []))

  const toast = useToast()

  const {buildRequest} = useChatRequestBuilder({doc, activeThread, provider, selectedModel, aiSettings, getMessages})
  const {registerStream, cancelStream, beginAcc, dropAcc, stopAndCommit, bumpGen, isCurrentGen, respondFixProposal} =
    useChatStreamEngine({
      doc,
      provider,
      selectedModel,
      getMessages,
    })

  const executeSend = useCallback(
    async (text: string, overrideFiles?: string[], excludeContext?: boolean, includeHistory = false) => {
      if (!doc || !activeThread) return
      // Per-thread guard: one stream per thread (enforced structurally by the
      // store's streams map, but checked here to no-op a double-send race).
      if (useChatStore.getState().streams[activeThread.id]) return
      // Global cap (mirrors backend maxConcurrentStreamsPerScope): reject early
      // with a toast so the user gets immediate feedback rather than a rejected
      // POST after a round-trip.
      if (!useChatStore.getState().canStartStream()) {
        toast.warning('Chats are busy', {
          description: `${MAX_CONCURRENT_STREAMS} chats are already generating — wait for one to finish or stop it, then try again.`,
        })
        return
      }
      const req = buildRequest(text, overrideFiles, excludeContext, includeHistory) as ChatRequest | null
      if (!req) return

      const isFirstMessage = getMessages(activeThread.id).length === 0

      const userMsg: ChatMessage = {
        id: crypto.randomUUID(),
        role: 'user',
        content: text,
        timestamp: new Date().toISOString(),
        contextBlockId: activeThread.contextBlockId ?? undefined,
      }
      appendMessage(activeThread.id, userMsg)
      if (isFirstMessage && !activeThread.title && text.trim()) {
        updateThread(activeThread.id, {title: text.replace(/\n+/g, ' ').trim().slice(0, 50)})
      }
      // No save-on-send — the backend reconstructs history; save-on-done
      // persists the full user+assistant pair.

      const msgId = crypto.randomUUID()
      const threadId = activeThread.id
      const myGen = bumpGen(threadId)
      const sid = crypto.randomUUID()
      startStream(threadId, sid, msgId)
      beginAcc(sid, threadId)

      try {
        // Subscribe + wait for the SSE connection to be 'open' BEFORE creating
        // the stream so no chunk is lost; registerStream returns false if the
        // hook was unmounted mid-registration — then do NOT create the stream.
        const registered = await registerStream(sid, /*begin*/ false)
        if (!registered || !isCurrentGen(threadId, myGen)) {
          dropAcc(sid)
          cancelStream(sid)
          endStream(threadId)
          return
        }
        // Create the stream with the client-generated ID. The backend stores it
        // and — because clientStreamId is set — closes ctl.started immediately,
        // so the worker emits over SSE to the already-subscribed listener.
        req.clientStreamId = sid
        const returnedId = await chatApi.streamChatMessage(req)
        if (!isCurrentGen(threadId, myGen)) {
          // Cancelled while the create POST was in flight; tear down the stream
          // the backend just recorded and clear the slot.
          dropAcc(sid)
          cancelStream(sid)
          endStream(threadId)
          return
        }
        if (!returnedId) {
          dropAcc(sid)
          cancelStream(sid)
          endStream(threadId)
          appendMessage(threadId, {
            id: crypto.randomUUID(),
            role: 'assistant',
            content: '*Error: No response stream was created. Please check your connection and try again.*',
            timestamp: new Date().toISOString(),
            provider,
            model: selectedModel,
            finishReason: 'error',
          } as ChatMessage)
          return
        }
        // Stream is live; chunks (or a fail-fast error/done) arrive over SSE and
        // are dispatched by the handlers registered in useChatStreamEngine.
      } catch (e: unknown) {
        if (!isCurrentGen(threadId, myGen)) return
        const errMsg = e instanceof Error ? e.message : String(e) || 'Failed to send message'
        // Classify by the envelope's machine-readable code first, falling back
        // to the message regex for older backends. Capacity errors are
        // transient — toast + clean up, no *Error* bubble in history.
        const apiErr = e instanceof ApiError ? e : null
        const capacityReached = apiErr?.code === 'CHAT_CAPACITY_REACHED' || errMsg.includes('too many chat responses')
        const budgetExceeded =
          apiErr?.code === 'AI_BUDGET_EXCEEDED' || (/budget/i.test(errMsg) && !/check unavailable/i.test(errMsg))
        if (capacityReached) {
          toast.warning('Chats are busy', {
            description: `${MAX_CONCURRENT_STREAMS} chats are already generating — wait for one to finish or stop it, then try again.`,
          })
        } else if (budgetExceeded) {
          // Daily AI budget hit: surface a dedicated, actionable message instead
          // of a raw "Error: daily AI budget exceeded ($X / $Y)" bubble — the
          // user needs to know it resets at midnight, not that something broke.
          const amounts = errMsg.match(/\$[0-9.]+\s*\/\s*\$[0-9.]+/)
          const detail = amounts ? ` (${amounts[0]})` : ''
          toast.warning("You've reached today's AI budget" + detail, {
            description:
              'AI requests are capped per day per organization. The budget resets at midnight UTC; contact an admin to raise it.',
          })
          appendMessage(threadId, {
            id: crypto.randomUUID(),
            role: 'assistant',
            content: `🛑 **Daily AI budget reached${detail}.** It resets at midnight UTC. Contact an admin to adjust the limit.`,
            timestamp: new Date().toISOString(),
            provider,
            model: selectedModel,
            finishReason: 'error',
          } as ChatMessage)
        } else {
          // Surface any other create failure as an error bubble so the user
          // knows their message didn't go through.
          logger.warn('chat stream create failed', e)
          appendMessage(threadId, {
            id: crypto.randomUUID(),
            role: 'assistant',
            content: `*Error: ${errMsg}*`,
            timestamp: new Date().toISOString(),
            provider,
            model: selectedModel,
            finishReason: 'error',
          } as ChatMessage)
        }
        // Tear down the SSE subscription we opened and cancel any backend stream
        // that the create POST may have recorded before throwing.
        dropAcc(sid)
        cancelStream(sid)
        endStream(threadId)
      }
    },
    [
      doc,
      activeThread,
      buildRequest,
      appendMessage,
      getMessages,
      startStream,
      endStream,
      registerStream,
      provider,
      selectedModel,
      toast,
      bumpGen,
      isCurrentGen,
      updateThread,
      dropAcc,
      beginAcc,
      cancelStream,
    ],
  )

// Queued follow-up drain (U1.6): when the ACTIVE thread's stream ends,
  // auto-send the message composed while it was streaming. The edge detect
  // (not a subscription) means switching threads mid-stream doesn't misfire.
  const wasActiveStreaming = useRef(false)
  useEffect(() => {
    if (wasActiveStreaming.current && !isCurrentThreadStreaming) {
      const tid = activeThread?.id
      const st = useChatStore.getState()
      const queued = tid ? st.queuedByThread[tid] : undefined
      // Guard BEFORE take (F1.7): takeQueuedMessage deletes, so sending it
      // into executeSend's early-returns (stream cap / no doc / empty request)
      // silently lost the message. Blocked sends stay queued — the chip keeps
      // its promise for the next drain.
      const blocked =
        !queued || !tid || !doc || !!st.streams[tid] || !st.canStartStream()
      if (!blocked) {
        st.takeQueuedMessage(tid)
        void executeSend(queued.text, queued.files.length ? queued.files : undefined, queued.excludeContext)
      }
    }
    wasActiveStreaming.current = isCurrentThreadStreaming
  }, [isCurrentThreadStreaming, activeThread, executeSend, doc])

  const handleSend = useCallback(
    (text: string, files: string[], excludeContext?: boolean) => {
      if (files.length > 0 && activeThreadId) {
        updateThread(activeThreadId, {selectedSourceFiles: files})
      }
      void executeSend(text, files.length > 0 ? files : undefined, excludeContext)
    },
    [executeSend, activeThreadId, updateThread],
  )

  // Queue a follow-up while this thread is streaming (U1.6). Replaces the
  // previous queued message (one per thread) — the drain effect above sends
  // it the moment the stream ends.
  const handleQueue = useCallback(
    (text: string, files: string[], excludeContext?: boolean) => {
      if (!activeThreadId || !text.trim()) return
      useChatStore.getState().queueMessage(activeThreadId, {text: text.trim(), files, excludeContext})
    },
    [activeThreadId],
  )
  const cancelQueued = useCallback(() => {
    if (activeThreadId) useChatStore.getState().clearQueuedMessage(activeThreadId)
  }, [activeThreadId])
  const queuedForActiveThread = useChatStore(
    s => (s.activeThreadId ? s.queuedByThread[s.activeThreadId] : undefined),
  )

  const handlePreviewContext = useCallback(
    async (text: string, files: string[], excludeContext?: boolean) => {
      if (files.length > 0 && activeThreadId) {
        updateThread(activeThreadId, {selectedSourceFiles: files})
      }
      const req = buildRequest(text, files.length > 0 ? files : undefined, excludeContext)
      if (!req) return
      try {
        const preview = (await chatApi.previewContext(req as ChatRequest)) as ContextPreview
        setContextPreview(preview)
        setPendingMessage(text)
        setPendingExcludeContext(excludeContext)
      } catch {
        void executeSend(text, files.length > 0 ? files : undefined, excludeContext)
      }
    },
    [buildRequest, executeSend, activeThreadId, updateThread],
  )

  const handleResend = useCallback(() => {
    if (!doc || !activeThread || isCurrentThreadStreaming) return
    const msgs = getMessages(activeThread.id)
    if (msgs.length < 2) return
    const lastMsg = msgs[msgs.length - 1]
    if (lastMsg.role !== 'assistant') return
    let lastUserContent = ''
    for (let i = msgs.length - 2; i >= 0; i--) {
      if (msgs[i].role === 'user') {
        lastUserContent = msgs[i].content
        removeMessage(activeThread.id, msgs[i].id)
        break
      }
    }
    removeMessage(activeThread.id, lastMsg.id)
    // Resend truncated the local history; send it explicitly so the backend
    // uses the truncated view instead of its (still-complete) stored copy.
    if (lastUserContent) void executeSend(lastUserContent, undefined, false, true)
  }, [doc, activeThread, isCurrentThreadStreaming, getMessages, removeMessage, executeSend])

  const handleExport = useCallback(() => {
    if (!activeThread) return
    const messages = getMessages(activeThread.id) as ChatMessage[]
    if (messages.length === 0) return
    const title = activeThread.title || 'Chat conversation'
    downloadTextFile(safeFilename(title), conversationToMarkdown(title, messages))
  }, [activeThread, getMessages])

  // handleClearThread empties the active thread locally and on the backend,
  // using the same (flowId, scope) key the save/get path uses. Backing the
  // /clear slash command; a no-op if a stream is in flight.
  const handleClearThread = useCallback(() => {
    if (!doc || !activeThread || isCurrentThreadStreaming) return
    clearThreadMessages(activeThread.id)
    chatApi.clearConversation(doc.id, activeThread.contextBlockId || 'flow').catch(err => {
      logger.warn('Failed to clear conversation', err)
    })
  }, [doc, activeThread, isCurrentThreadStreaming, clearThreadMessages])

  const handleCancelStream = useCallback(() => {
    const tid = useChatStore.getState().activeThreadId
    if (!tid) return
    const slot = useChatStore.getState().streams[tid]
    if (!slot) return
    const sid = slot.streamId
    // Bump the per-thread gen so any in-flight executeSend sees it's stale
    // and self-cancels.
    bumpGen(tid)
    if (sid && sid !== 'pending') {
      // stopAndCommit cancels the SSE sub + backend stream, keeping whatever
      // text had streamed before the stop instead of discarding it.
      stopAndCommit(sid, tid, slot.messageId)
    }
    endStream(tid)
  }, [bumpGen, endStream, stopAndCommit])

  // Tear down the SSE sub before delegating — closeThread only cancels the
  // backend stream and clears the store slot.
  const handleCloseThread = useCallback(
    (threadId: string) => {
      const slot = useChatStore.getState().streams[threadId]
      if (slot?.streamId && slot.streamId !== 'pending') {
        dropAcc(slot.streamId)
        cancelStream(slot.streamId)
      }
      threadActions.handleCloseThread(threadId)
    },
    [threadActions, dropAcc, cancelStream],
  )

  return {
    doc,
    selectedBlockId,
    activeThreadId,
    activeThread,
    activeThreadMessages,
    flowThreads,
    isStreaming,
    isCurrentThreadStreaming,
    showThinking,
    sourceFiles,
    contextPreview,
    pendingMessage,
    toolStatus,
    fixProposals,
    respondFixProposal,
    ...threadActions,
    handleCloseThread,
    handleSend,
    handleQueue,
    cancelQueued,
    queuedForActiveThread,
    handlePreviewContext,
    handleResend,
    handleExport,
    handleClearThread,
    handleCancelStream,
    clearContextPreview: () => {
      setContextPreview(null)
      setPendingMessage(null)
      setPendingExcludeContext(undefined)
    },
    confirmContextPreview: () => {
      const msg = pendingMessage
      const exclude = pendingExcludeContext
      setContextPreview(null)
      setPendingMessage(null)
      setPendingExcludeContext(undefined)
      if (msg) void executeSend(msg, undefined, exclude)
    },
  }
}
