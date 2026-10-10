import {useTranslation} from 'react-i18next'
import {useState, useCallback, useMemo} from 'react'
import {
  useChatStore,
  conversationKey,
  threadPinKey,
  MAX_CONCURRENT_STREAMS,
  type ApprovalCard,
} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi, ApiError} from '@/api'
import {useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import {CHAT_ERROR_CODES} from '@/lib/chatErrors'
import {conversationToMarkdown, downloadTextFile, safeFilename, type ExportModelName} from '@/lib/chatExport'
import {useChatConversations} from './useChatConversations'
import {useChatThreads} from './useChatThreads'
import {useChatRequestBuilder} from './useChatRequestBuilder'
import {useChatStream} from '../chatStreamContext'
import type {TurnModel} from './useChatStreamEngine'
import type {ChatMessage, ContextPreview} from '@/types'

interface UseAIChatOptions {
  // The ACTIVE thread's provider/model, as resolved per thread
  // (useProviderSetup): every turn this panel sends on the active thread
  // goes there, and a queued or previewed message keeps the pair it was
  // composed under.
  turnModel: TurnModel
  // How an exported assistant turn names its model (the chat resolves labels
  // against the provider listing); defaults to the stored label.
  exportModelName?: ExportModelName
  // Nothing can be sent right now — the composer's guidance holds (the
  // thread's Azure AI Foundry scope has no model to chat with). Every way of
  // sending refuses meanwhile, not just the composer: suggested prompts,
  // templates, Retry/Regenerate, the queue and the preview would otherwise
  // go out with no model at all.
  sendBlocked?: boolean
}

const EMPTY_ARRAY: ChatMessage[] = []
// Stable fallback for the approval-card selector: a fresh `[]` per call made
// the selector's result differ on EVERY store update (Zustand compares with
// Object.is), re-rendering the whole panel at 60fps while any other thread
// streamed.
const EMPTY_APPROVALS: ApprovalCard[] = []

interface PendingPreview {
  // The thread the preview was built for: the user may switch threads while
  // the dialog is open, and Send must still go where the preview said.
  threadId: string
  // …and with the model the preview was built for: the dialog shows that
  // model's request, whatever is picked while it is open.
  turn: TurnModel & {pinnedAs: string}
  text: string
  files: string[]
  excludeContext?: boolean
  // Resets the composer once the previewed message is actually sent.
  onConfirmed?: () => void
}

// useAIChat is the chat PANEL's view of the conversation: the active thread,
// its messages and stream state, and the actions the panel's controls call.
// Sending, stopping and every stream callback live in the app-wide engine
// (useChatStream) — the panel can mount and unmount without touching them.
export function useAIChat({turnModel, exportModelName, sendBlocked = false}: UseAIChatOptions) {
  const {t} = useTranslation('chat')
  const doc = useFlowStore(s => s.document)
  const readOnly = useFlowStore(s => s.readOnly)

  const {provider, model: selectedModel} = turnModel
  const aiSettings = useSettingsStore(s => s.settings.ai)

  const getMessages = useChatStore(s => s.getMessages)
  const removeMessage = useChatStore(s => s.removeMessage)
  const clearThreadMessages = useChatStore(s => s.clearThreadMessages)

  const threads = useChatStore(s => s.threads)
  const activeThreadId = useChatStore(s => s.activeThreadId)
  const activeThreadMessages = useChatStore(s =>
    s.activeThreadId ? (s.conversations.get(s.activeThreadId) ?? EMPTY_ARRAY) : EMPTY_ARRAY,
  )

  const flowThreads = useMemo(() => (doc ? threads.filter(t => t.flowId === doc.id) : []), [doc, threads])
  const activeThread = useMemo(() => threads.find(t => t.id === activeThreadId) ?? null, [threads, activeThreadId])

  const toast = useToast()
  const engine = useChatStream()

  const {sourceFiles, historyState, retryHistoryLoad} = useChatConversations({
    doc,
    flowThreads,
    activeThreadId,
    // A conversation load that reports an in-flight stream (a reload
    // happened mid-turn) re-attaches through the app-wide engine.
    resumeActiveStream: engine.resumeAfterReload,
  })
  const threadActions = useChatThreads({doc, activeThreadId, sourceFiles})

  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingPreview, setPendingPreview] = useState<PendingPreview | null>(null)

  // Streaming state is per-thread (chatStore.streams); this hook surfaces the
  // ACTIVE thread's slot. Narrow selectors keep per-chunk re-renders scoped:
  // only slot fields used here trigger updates.
  const isStreaming = useChatStore(s => Object.keys(s.streams).length > 0)
  const isCurrentThreadStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const showThinking = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]?.isThinking))
  const toolStatus = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolStatus ?? null) : null))
  const approvals = useChatStore(s =>
    s.activeThreadId ? (s.streams[s.activeThreadId]?.approvals ?? EMPTY_APPROVALS) : EMPTY_APPROVALS,
  )

  const {buildRequest} = useChatRequestBuilder({doc, activeThread, provider, selectedModel, aiSettings, getMessages})

  // executeSend runs a turn on the ACTIVE thread with that thread's
  // provider/model — every send path goes through it (see sendBlocked).
  // Reports (synchronously) whether the message was accepted. `restore`: a
  // resend's messages from before it took the last exchange off.
  const executeSend = useCallback(
    (
      text: string,
      overrideFiles?: string[],
      excludeContext?: boolean,
      includeHistory = false,
      restore?: readonly ChatMessage[],
    ): boolean => {
      if (!doc || !activeThread || sendBlocked) return false
      return engine.send(activeThread.id, text, {
        ...turnModel,
        files: overrideFiles,
        excludeContext,
        includeHistory,
        ...(restore ? {restore} : {}),
      })
    },
    [doc, activeThread, engine, turnModel, sendBlocked],
  )

  // @-mentioned files are a PER-MESSAGE override (the request builder's
  // overrideFiles). They are not written into the thread's persistent
  // selection: doing so replaced whatever the user had picked in the source
  // file picker, and every later turn silently lost that context.
  const handleSend = useCallback(
    (text: string, files: string[], excludeContext?: boolean) => {
      executeSend(text, files.length > 0 ? files : undefined, excludeContext)
    },
    [executeSend],
  )

  // Queue a follow-up while this thread is streaming. Replaces the
  // previous queued message (one per thread); the stream host sends it the
  // moment the thread is idle, with the thread's provider/model as they are
  // now — a pick made before it drains does not redirect it.
  const handleQueue = useCallback(
    (text: string, files: string[], excludeContext?: boolean) => {
      if (!activeThreadId || !text.trim() || sendBlocked) return
      const st = useChatStore.getState()
      const pinnedAs = threadPinKey(st.threads.find(th => th.id === activeThreadId))
      st.queueMessage(activeThreadId, {text: text.trim(), files, excludeContext, ...turnModel, pinnedAs})
    },
    [activeThreadId, turnModel, sendBlocked],
  )
  const cancelQueued = useCallback(() => {
    if (activeThreadId) useChatStore.getState().clearQueuedMessage(activeThreadId)
  }, [activeThreadId])
  const queuedForActiveThread = useChatStore(s => (s.activeThreadId ? s.queuedByThread[s.activeThreadId] : undefined))

  // The preview is a consent step — "see exactly what goes to the AI
  // provider" — so it fails CLOSED: a preview that can't be built or loaded
  // tells the user and sends nothing (it used to send anyway).
  const handlePreviewContext = useCallback(
    async (text: string, files: string[], excludeContext?: boolean, onConfirmed?: () => void) => {
      if (!activeThread || sendBlocked) return
      const threadId = activeThread.id
      // The pair the preview is built for: confirming sends with exactly it
      // (and re-pins only a thread nobody re-picked meanwhile).
      const turn: TurnModel & {pinnedAs: string} = {
        ...turnModel,
        pinnedAs: threadPinKey(useChatStore.getState().threads.find(th => th.id === threadId)),
      }
      const req = buildRequest(text, files.length > 0 ? files : undefined, excludeContext)
      if (!req) return
      try {
        const preview = (await chatApi.previewContext(req)) as ContextPreview
        setContextPreview(preview)
        setPendingPreview({threadId, turn, text, files, excludeContext, onConfirmed})
      } catch (err) {
        logger.warn('Context preview failed', err)
        if (err instanceof ApiError && err.code === CHAT_ERROR_CODES.modelNotAvailable) {
          // The model went away after the picker listed it: refetch the
          // listing and raise the thread's notice, as a refused send does.
          const store = useChatStore.getState()
          store.bumpProviderEpoch()
          if (turn.model) {
            store.setModelRefusal(threadId, {
              provider: turn.provider,
              model: turn.model,
              ...(turn.modelLabel ? {modelLabel: turn.modelLabel} : {}),
              epoch: useChatStore.getState().providerEpoch,
            })
          }
          toast.error(t('errors.previewModelNotAvailable'))
          return
        }
        toast.error(t('errors.previewFailed'))
      }
    },
    [activeThread, buildRequest, toast, t, turnModel, sendBlocked],
  )

  const handleResend = useCallback(() => {
    // Refused BEFORE the exchange is taken off (see sendBlocked).
    if (!doc || !activeThread || isCurrentThreadStreaming || sendBlocked) return
    // Resend truncates the local history and sends it as the turn's history;
    // with the server's copy not loaded, the local view is incomplete.
    if (!useChatStore.getState().isHistorySynced(activeThread.id)) {
      toast.warning(t('errors.historyNotReady'))
      return
    }
    const msgs = getMessages(activeThread.id)
    if (msgs.length < 2) return
    const lastMsg = msgs[msgs.length - 1]
    if (!lastMsg) return
    if (lastMsg.role !== 'assistant') return
    if (!useChatStore.getState().canStartStream()) {
      // Checked BEFORE the exchange is removed: removing it first and then
      // hitting the concurrency cap lost it.
      toast.warning(t('errors.busyTitle'), {description: t('errors.busyBody', {count: MAX_CONCURRENT_STREAMS})})
      return
    }
    let lastUserContent = ''
    for (let i = msgs.length - 2; i >= 0; i--) {
      const m = msgs[i]
      if (m?.role === 'user') {
        lastUserContent = m.content
        removeMessage(activeThread.id, m.id)
        break
      }
    }
    removeMessage(activeThread.id, lastMsg.id)
    // Resend truncated the local history; send it explicitly so the backend
    // uses the truncated view instead of its (still-complete) stored copy.
    // A send the server refuses puts the exchange back (SendOptions.restore).
    if (lastUserContent) executeSend(lastUserContent, undefined, false, true, msgs)
  }, [doc, activeThread, isCurrentThreadStreaming, sendBlocked, getMessages, removeMessage, executeSend, toast, t])

  const handleExport = useCallback(() => {
    if (!activeThread) return
    const messages = getMessages(activeThread.id) as ChatMessage[]
    if (messages.length === 0) return
    const title = activeThread.title || t('export.defaultTitle')
    downloadTextFile(safeFilename(title), conversationToMarkdown(title, messages, exportModelName))
  }, [activeThread, getMessages, t, exportModelName])

  // handleClearThread empties the active thread on the backend, then locally,
  // using the same (flowId, conversation key) the save/get path uses. Backing
  // the /clear slash command and Ctrl+L; a no-op while a stream is in flight.
  // Server first: a failed clear used to empty the panel while the model kept
  // answering from the stored history the user thought was deleted.
  const handleClearThread = useCallback(() => {
    if (!doc || !activeThread || isCurrentThreadStreaming || readOnly) return
    const threadId = activeThread.id
    chatApi
      .clearConversation(activeThread.flowId, conversationKey(activeThread))
      .then(() => {
        // Re-checked on return: a turn sent meanwhile must not be wiped, and a
        // thread closed meanwhile must not come back.
        const st = useChatStore.getState()
        if (!st.threads.some(th => th.id === threadId) || st.streams[threadId]) return
        clearThreadMessages(threadId)
      })
      .catch(err => {
        logger.warn('Failed to clear conversation', err)
        toast.error(t('errors.clearFailed'))
      })
  }, [doc, activeThread, isCurrentThreadStreaming, readOnly, clearThreadMessages, toast, t])

  const handleCancelStream = useCallback(() => {
    const tid = useChatStore.getState().activeThreadId
    if (tid) engine.stop(tid)
  }, [engine])

  // Drop the thread's in-flight reply (uncommitted) before closing it.
  const handleCloseThread = useCallback(
    (threadId: string) => {
      engine.abandon(threadId)
      threadActions.handleCloseThread(threadId)
    },
    [engine, threadActions],
  )

  const respondFixProposal = engine.respondFixProposal

  const clearContextPreview = useCallback(() => {
    setContextPreview(null)
    setPendingPreview(null)
  }, [])

  const confirmContextPreview = useCallback(() => {
    const pending = pendingPreview
    setContextPreview(null)
    setPendingPreview(null)
    if (!pending) return
    // Sent to the thread the preview was built for, with the model it was
    // built for, and the composer reset as soon as the message is on screen
    // — not after the network round-trips, during which Enter used to queue
    // a duplicate — and only while the composer still shows that thread.
    const accepted = engine.send(pending.threadId, pending.text, {
      ...pending.turn,
      files: pending.files.length > 0 ? pending.files : undefined,
      excludeContext: pending.excludeContext,
    })
    if (accepted && useChatStore.getState().activeThreadId === pending.threadId) pending.onConfirmed?.()
  }, [pendingPreview, engine])

  return {
    doc,
    readOnly,
    activeThreadId,
    activeThread,
    activeThreadMessages,
    flowThreads,
    isStreaming,
    isCurrentThreadStreaming,
    showThinking,
    sourceFiles,
    historyState,
    retryHistoryLoad,
    contextPreview,
    pendingMessage: pendingPreview?.text ?? null,
    toolStatus,
    approvals,
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
    clearContextPreview,
    confirmContextPreview,
  }
}
