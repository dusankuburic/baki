import {useState, useCallback, useRef, useEffect, useMemo} from 'react'
import {useChatStore, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi} from '@/api'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import {utf8ByteLength} from '@/lib/utf8'
import {conversationToMarkdown, downloadTextFile, safeFilename} from '@/lib/chatExport'
import {useChatConversations} from './useChatConversations'
import {useChatThreads} from './useChatThreads'
import type {ChatMessage, ContextPreview, ChatRequest} from '@/types'

interface UseAIChatOptions {
  selectedModel: string
}

const EMPTY_ARRAY: ChatMessage[] = []

// StreamAcc is one in-flight stream's local accumulation state: the growing
// text plus the RAF batching bookkeeping. Keyed by streamId in a ref so
// multiple threads can stream concurrently; the map entry doubles as the
// stale-event guard — a late event whose streamId has no entry is ignored.
interface StreamAcc {
  threadId: string
  text: string
  raf: number | null
  first: boolean
}

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
  const setStreamMeta = useChatStore(s => s.setStreamMeta)
  const updateStream = useChatStore(s => s.updateStream)

  const threads = useChatStore(s => s.threads)
  const activeThreadId = useChatStore(s => s.activeThreadId)
  const activeThreadMessages = useChatStore(s =>
    s.activeThreadId ? (s.conversations.get(s.activeThreadId) ?? EMPTY_ARRAY) : EMPTY_ARRAY
  )

  const flowThreads = useMemo(
    () => doc ? threads.filter(t => t.flowId === doc.id) : [],
    [doc, threads],
  )
  const activeThread = useMemo(
    () => threads.find(t => t.id === activeThreadId) ?? null,
    [threads, activeThreadId],
  )

  const {sourceFiles} = useChatConversations({doc, flowThreads, activeThreadId})
  const threadActions = useChatThreads({doc, activeThreadId, sourceFiles})

  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingMessage, setPendingMessage] = useState<string | null>(null)

  // Streaming state is per-thread (chatStore.streams); the hook surfaces the
  // ACTIVE thread's slot under the pre-existing names. Narrow selectors keep
  // per-chunk re-renders scoped: only slot fields used here trigger updates.
  const isStreaming = useChatStore(s => Object.keys(s.streams).length > 0)
  const isCurrentThreadStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const showThinking = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]?.isThinking))
  const toolStatus = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolStatus ?? null) : null))

  const providerRef = useRef(provider)
  useEffect(() => { providerRef.current = provider })
  const docRef = useRef(doc)
  useEffect(() => { docRef.current = doc })
  const selectedModelRef = useRef(selectedModel)
  useEffect(() => { selectedModelRef.current = selectedModel })

  const toast = useToast()

  const streamAccRef = useRef(new Map<string, StreamAcc>())
  const teardownRef = useRef<(streamId: string) => void>(() => {})
  const cancelRef = useRef<(streamId: string) => void>(() => {})

  // threadGenRef is a per-thread generation token (replaces the old single
  // global sendGenRef). Each executeSend for a thread bumps its token; a
  // stale send (user cancelled, closed the thread, or sent again in the same
  // thread) detects it's no longer current after an await and self-cancels.
  // Per-thread is required now that several threads can stream concurrently.
  const threadGenRef = useRef(new Map<string, number>())
  const bumpGen = useCallback((threadId: string) => {
    const next = (threadGenRef.current.get(threadId) ?? 0) + 1
    threadGenRef.current.set(threadId, next)
    return next
  }, [])
  const isCurrentGen = useCallback((threadId: string, gen: number) =>
    threadGenRef.current.get(threadId) === gen, [])

  const flushAcc = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    acc.raf = null
    // One atomic store update (text + tokens) instead of two set() calls —
    // halves per-frame subscriber notifications during streaming.
    updateStream(acc.threadId, {text: acc.text, tokens: Math.ceil(acc.text.length / 4)})
  }, [updateStream])

  const onChunk = useCallback((text: string, streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    acc.text += text
    if (!acc.first) {
      // First chunk: flush synchronously so the slot text appears in the same
      // React render that clears the thinking indicator — no empty-frame gap.
      acc.first = true
      updateStream(acc.threadId, {text: acc.text, isThinking: false, tokens: 1})
    } else if (acc.raf === null) {
      acc.raf = requestAnimationFrame(() => flushAcc(streamId))
    }
  }, [flushAcc, updateStream])

  const onReplace = useCallback((text: string, streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    acc.text = text
    acc.first = true
    updateStream(acc.threadId, {text, isThinking: false, tokens: Math.ceil(text.length / 4)})
  }, [updateStream])

  const onToolStatus = useCallback((label: string, streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    setStreamMeta(acc.threadId, {isThinking: false, toolStatus: label})
  }, [setStreamMeta])

  // takeAcc removes and returns a finished stream's accumulation state,
  // cancelling any pending flush. The store slot must still match the
  // streamId — a thread closed mid-stream drops its slot, and its result is
  // then discarded rather than committed to a dead thread.
  const takeAcc = useCallback((streamId: string): StreamAcc | null => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return null
    streamAccRef.current.delete(streamId)
    if (acc.raf !== null) {
      cancelAnimationFrame(acc.raf)
      acc.raf = null
    }
    teardownRef.current(streamId)
    const slot = useChatStore.getState().streams[acc.threadId]
    if (!slot || slot.streamId !== streamId) return null
    return acc
  }, [])

  // dropAcc clears an acc entry on a send that never completed (stale-gen
  // return, create-POST failure). It cancels any pending RAF so a straggler
  // flush can't fire after the slot is gone. Unlike takeAcc it does NOT touch
  // the store slot or tear down SSE (the caller does cancelRef/endStream).
  const dropAcc = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    if (acc.raf !== null) cancelAnimationFrame(acc.raf)
    streamAccRef.current.delete(streamId)
  }, [])

  // commitAssistantMessage appends the streamed text as a permanent assistant
  // message and persists the thread. Shared by the natural-completion path
  // (onDone) and the user-stop path (handleCancelStream) so an interrupted
  // answer is kept, not discarded. Persisted messages are filtered of the
  // transient error bubbles so reloaded history isn't littered with them.
  const commitAssistantMessage = useCallback((
    threadId: string,
    messageId: string | undefined,
    content: string,
    opts: {tokensIn?: number; tokensOut?: number; finishReason: ChatMessage['finishReason']},
  ) => {
    const curDoc = docRef.current
    const msg: ChatMessage = {
      id: messageId || crypto.randomUUID(),
      role: 'assistant',
      content,
      timestamp: new Date().toISOString(),
      provider: providerRef.current,
      model: selectedModelRef.current,
      tokensIn: opts.tokensIn,
      tokensOut: opts.tokensOut,
      finishReason: opts.finishReason,
    }
    if (!curDoc) return
    appendMessage(threadId, msg)
    const thread = useChatStore.getState().threads.find(t => t.id === threadId)
    if (!thread) return
    const persistable = (getMessages(threadId) as ChatMessage[]).filter(m => m.finishReason !== 'error')
    chatApi.saveConversation(curDoc.id, thread.contextBlockId || 'flow', persistable).catch((err) => { logger.warn('Failed to save conversation', err) })
    if (opts.tokensIn != null || opts.tokensOut != null) {
      updateThread(threadId, {
        tokensIn: (thread.tokensIn ?? 0) + (opts.tokensIn ?? 0),
        tokensOut: (thread.tokensOut ?? 0) + (opts.tokensOut ?? 0),
      })
    }
  }, [appendMessage, getMessages, updateThread])

  const onDone = useCallback((tokensOut: number, tokensIn: number, streamId: string) => {
    const acc = takeAcc(streamId)
    if (!acc) return
    const slot = useChatStore.getState().streams[acc.threadId]
    commitAssistantMessage(acc.threadId, slot?.messageId, acc.text, {tokensIn, tokensOut, finishReason: 'stop'})
    endStream(acc.threadId)
  }, [commitAssistantMessage, endStream, takeAcc])

  const onError = useCallback((error: string, streamId: string) => {
    const acc = takeAcc(streamId)
    if (!acc) return
    const curDoc = docRef.current
    const slot = useChatStore.getState().streams[acc.threadId]
    const displayContent = acc.text
      ? acc.text + '\n\n---\n*Error: ' + error + '*'
      : '*Error: ' + error + '*'
    const msg: ChatMessage = {
      id: slot?.messageId || crypto.randomUUID(),
      role: 'assistant',
      content: displayContent,
      timestamp: new Date().toISOString(),
      provider: providerRef.current,
      model: selectedModelRef.current,
      finishReason: 'error',
    }
    if (curDoc) appendMessage(acc.threadId, msg)
    endStream(acc.threadId)
  }, [appendMessage, endStream, takeAcc])

  // onAppend adds a delta-resume tail to the stream's accumulated text and
  // flushes it synchronously to the slot (no RAF — a resume delivers a known
  // tail after a reconnect, not a stream of live chunks). Used by
  // useStreamingMessage.resumeInto in 'delta' mode.
  const onAppend = useCallback((delta: string, streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc || !delta) return
    acc.text += delta
    acc.first = true
    if (acc.raf !== null) { cancelAnimationFrame(acc.raf); acc.raf = null }
    updateStream(acc.threadId, {text: acc.text, isThinking: false, tokens: Math.ceil(acc.text.length / 4)})
  }, [updateStream])

  // getAccLength reports how many UTF-8 BYTES the client already holds for a
  // stream, so a delta-resume can request only the tail from the backend. The
  // backend slices its Go string buffer by BYTE offset, so the client must send
  // a byte length — NOT JS string .length (UTF-16 code units), which mismatches
  // on any non-ASCII content (emoji/CJK/accented) and corrupts the resumed tail.
  const getAccLength = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    return acc ? utf8ByteLength(acc.text) : 0
  }, [])

  const handler = useMemo(() => ({
    onChunk, onReplace, onDone, onError, onToolStatus, onAppend, getAccLength,
  }), [onChunk, onReplace, onDone, onError, onToolStatus, onAppend, getAccLength])

  const {registerStream, cancel, teardownStream} = useStreamingMessage(handler)
  useEffect(() => { teardownRef.current = teardownStream }, [teardownStream])
  useEffect(() => { cancelRef.current = cancel }, [cancel])

  // buildRequest assembles the chat request. By default it OMITS `messages`
  // (history): the backend reconstructs it from its conversation store, so the
  // client no longer re-sends the full history each turn (~30KB saved/request).
  // Set includeHistory=true when the client has locally truncated history
  // (resend/edit) — the backend then uses the provided slice as-is instead of
  // its stored copy.
  const buildRequest = useCallback((text: string, overrideFiles?: string[], excludeContext?: boolean, includeHistory = false) => {
    if (!doc || !activeThread) return null
    const providerConfig = aiSettings.providers[provider as keyof typeof aiSettings.providers]
    const currentThread = useChatStore.getState().threads.find(t => t.id === activeThread.id)
    let filesToUse = currentThread?.selectedSourceFiles || []
    if (overrideFiles !== undefined && overrideFiles.length > 0) {
      filesToUse = overrideFiles
    }
    return {
      flowId: doc.id,
      provider,
      model: selectedModel || providerConfig?.defaultModel || '',
      // Omit `messages` unless the caller needs to override server-side history.
      ...(includeHistory ? {
        messages: getMessages(activeThread.id).map((m: ChatMessage) => ({id: m.id, role: m.role, content: m.content, timestamp: m.timestamp})),
      } : {}),
      userMessage: text,
      // ALWAYS send contextBlockId: it is the server-side conversation-history
      // key (so a free-form / excludeContext turn on a block-scoped thread
      // still reconstructs the right history). excludeContext below gates only
      // flow/block CONTEXT INJECTION, not which conversation is loaded.
      contextBlockId: activeThread.contextBlockId || '',
      selectedSourceFiles: excludeContext ? undefined : (filesToUse.length > 0 ? filesToUse : undefined),
      temperature: providerConfig?.temperature ?? 0.3,
      maxTokens: providerConfig?.maxTokens ?? 4096,
      excludeContext: excludeContext ?? false,
      useTools: currentThread?.useTools ?? false,
    }
  }, [doc, activeThread, provider, selectedModel, aiSettings, getMessages])

  const executeSend = useCallback(async (text: string, overrideFiles?: string[], excludeContext?: boolean, includeHistory = false) => {
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
    // Note: no save-on-send — the backend reconstructs history from its store
    // (which holds the prior turn's state), and save-on-done persists the full
    // user+assistant pair after completion. This drops one full-history POST
    // per turn without changing on-success persistence.

    const msgId = crypto.randomUUID()
    const threadId = activeThread.id
    const myGen = bumpGen(threadId)
    // C-1: client-generated stream ID. The SSE listener is subscribed BEFORE
    // the create POST, so the backend can emit immediately (clientStreamId
    // handshake) with no /chat/begin round-trip. The acc is primed up front so
    // the first chunk finds its key, and the slot is reserved with the real sid
    // (no 'pending' phase) so the thinking indicator shows and a second send
    // for this thread is blocked from the start.
    const sid = crypto.randomUUID()
    startStream(threadId, sid, msgId)
    streamAccRef.current.set(sid, {threadId, text: '', raf: null, first: false})

    try {
      // Subscribe the listener + wait for the SSE connection to be 'open'
      // BEFORE creating the stream, so no chunk is lost when the backend emits.
      await registerStream(sid, /*begin*/ false)
      if (!isCurrentGen(threadId, myGen)) {
        dropAcc(sid)
        cancelRef.current(sid)
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
        cancelRef.current(sid)
        endStream(threadId)
        return
      }
      if (!returnedId) {
        dropAcc(sid)
        cancelRef.current(sid)
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
      // are dispatched by the handlers registered above.
    } catch (e: unknown) {
      if (!isCurrentGen(threadId, myGen)) return
      const errMsg = e instanceof Error ? e.message : String(e) || 'Failed to send message'
      // The concurrency cap (or any pre-stream-create failure) surfaces here as
      // a thrown error. Treat it as a transient condition: toast + clean up, do
      // NOT persist an *Error* bubble into the history for the cap class.
      if (errMsg.includes('too many chat responses')) {
        toast.warning('Chats are busy', {
          description: `${MAX_CONCURRENT_STREAMS} chats are already generating — wait for one to finish or stop it, then try again.`,
        })
      } else {
        appendMessage(threadId, {
          id: crypto.randomUUID(),
          role: 'assistant',
          content: '*Error: ' + errMsg + '*',
          timestamp: new Date().toISOString(),
          provider,
          model: selectedModel,
          finishReason: 'error',
        } as ChatMessage)
      }
      // Tear down the SSE subscription we opened and cancel any backend stream
      // that the create POST may have recorded before throwing. (B8 leak fix —
      // without cancelStream the backend goroutine holds a live provider
      // connection until the stream-cap timeout.)
      dropAcc(sid)
      cancelRef.current(sid)
      endStream(threadId)
    }
  }, [doc, activeThread, buildRequest, appendMessage, getMessages, startStream, endStream, registerStream, provider, selectedModel, toast, bumpGen, isCurrentGen, updateThread, dropAcc])

  const handleSend = useCallback((text: string, files: string[], excludeContext?: boolean) => {
    if (files.length > 0 && activeThreadId) {
       updateThread(activeThreadId, {selectedSourceFiles: files})
    }
    executeSend(text, files.length > 0 ? files : undefined, excludeContext)
  }, [executeSend, activeThreadId, updateThread])

  const handlePreviewContext = useCallback(async (text: string, files: string[], excludeContext?: boolean) => {
    if (files.length > 0 && activeThreadId) {
       updateThread(activeThreadId, {selectedSourceFiles: files})
    }
    const req = buildRequest(text, files.length > 0 ? files : undefined, excludeContext)
    if (!req) return
    try {
      const preview = await chatApi.previewContext(req as ChatRequest) as ContextPreview
      setContextPreview(preview)
      setPendingMessage(text)
    } catch {
      executeSend(text, files.length > 0 ? files : undefined, excludeContext)
    }
  }, [buildRequest, executeSend, activeThreadId, updateThread])

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
    if (lastUserContent) executeSend(lastUserContent, undefined, false, true)
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
    chatApi.clearConversation(doc.id, activeThread.contextBlockId || 'flow').catch((err) => { logger.warn('Failed to clear conversation', err) })
  }, [doc, activeThread, isCurrentThreadStreaming, clearThreadMessages])

  const handleCancelStream = useCallback(() => {
    const tid = useChatStore.getState().activeThreadId
    if (!tid) return
    const slot = useChatStore.getState().streams[tid]
    if (!slot) return
    const sid = slot.streamId
    // Bump the per-thread gen so any in-flight executeSend (still awaiting
    // streamChatMessage with a 'pending' sentinel) sees it's stale and
    // self-cancels once the real sid arrives.
    bumpGen(tid)
    if (sid && sid !== 'pending') {
      // Drop the acc (cancel any pending RAF flush) and tear down the SSE sub;
      // cancelRef does backend cancel + teardownStream in one call.
      const acc = streamAccRef.current.get(sid)
      if (acc) {
        if (acc.raf !== null) cancelAnimationFrame(acc.raf)
        streamAccRef.current.delete(sid)
        // Keep whatever was generated before the stop instead of discarding it.
        if (acc.text) {
          commitAssistantMessage(tid, slot.messageId, acc.text, {finishReason: 'interrupted'})
        }
      }
      cancelRef.current(sid)
    }
    endStream(tid)
  }, [bumpGen, endStream, commitAssistantMessage])

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
    ...threadActions,
    handleSend,
    handlePreviewContext,
    handleResend,
    handleExport,
    handleClearThread,
    handleCancelStream,
    clearContextPreview: () => { setContextPreview(null); setPendingMessage(null) },
    confirmContextPreview: () => {
      const msg = pendingMessage
      setContextPreview(null)
      setPendingMessage(null)
      if (msg) executeSend(msg)
    },
  }
}
