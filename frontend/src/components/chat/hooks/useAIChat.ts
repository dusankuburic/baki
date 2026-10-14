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
import {clearThreadConversation} from '@/lib/conversationSync'
import {CHAT_ERROR_CODES} from '@/lib/chatErrors'
import {conversationToMarkdown, downloadTextFile, safeFilename, type ExportModelName} from '@/lib/chatExport'
import {useChatConversations} from './useChatConversations'
import {useChatThreads} from './useChatThreads'
import {useChatRequestBuilder} from './useChatRequestBuilder'
import {useChatStream} from '../chatStreamContext'
import type {TurnModel} from './useChatStreamEngine'
import type {ChatMessage, ContextPreview} from '@/types'

interface UseAIChatOptions {
  turnModel: TurnModel
  exportModelName?: ExportModelName
  sendBlocked?: boolean
}

const EMPTY_ARRAY: ChatMessage[] = []
const EMPTY_APPROVALS: ApprovalCard[] = []

interface PendingPreview {
  threadId: string
  turn: TurnModel & {pinnedAs: string}
  text: string
  files: string[]
  excludeContext?: boolean
  onConfirmed?: () => void
}

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
    resumeActiveStream: engine.resumeAfterReload,
  })
  const threadActions = useChatThreads({doc, activeThreadId, sourceFiles})

  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingPreview, setPendingPreview] = useState<PendingPreview | null>(null)

  const isStreaming = useChatStore(s => Object.keys(s.streams).length > 0)
  const isCurrentThreadStreaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const showThinking = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]?.isThinking))
  const toolStatus = useChatStore(s => (s.activeThreadId ? (s.streams[s.activeThreadId]?.toolStatus ?? null) : null))
  const approvals = useChatStore(s =>
    s.activeThreadId ? (s.streams[s.activeThreadId]?.approvals ?? EMPTY_APPROVALS) : EMPTY_APPROVALS,
  )

  const {buildRequest} = useChatRequestBuilder({doc, activeThread, provider, selectedModel, aiSettings, getMessages})

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

  const handleSend = useCallback(
    (text: string, files: string[], excludeContext?: boolean) => {
      executeSend(text, files.length > 0 ? files : undefined, excludeContext)
    },
    [executeSend],
  )

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

  const handlePreviewContext = useCallback(
    async (text: string, files: string[], excludeContext?: boolean, onConfirmed?: () => void) => {
      if (!activeThread || sendBlocked) return
      const threadId = activeThread.id
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
    if (!doc || !activeThread || isCurrentThreadStreaming || sendBlocked) return
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
    if (lastUserContent) executeSend(lastUserContent, undefined, false, true, msgs)
  }, [doc, activeThread, isCurrentThreadStreaming, sendBlocked, getMessages, removeMessage, executeSend, toast, t])

  const handleExport = useCallback(() => {
    if (!activeThread) return
    const messages = getMessages(activeThread.id) as ChatMessage[]
    if (messages.length === 0) return
    const title = activeThread.title || t('export.defaultTitle')
    downloadTextFile(safeFilename(title), conversationToMarkdown(title, messages, exportModelName))
  }, [activeThread, getMessages, t, exportModelName])

  const handleClearThread = useCallback(() => {
    if (!doc || !activeThread || isCurrentThreadStreaming || readOnly) return
    const threadId = activeThread.id
    clearThreadConversation(threadId, activeThread.flowId, conversationKey(activeThread))
      .then(cleared => {
        if (!cleared) {
          toast.warning(t('errors.changedElsewhere'))
          return
        }
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
