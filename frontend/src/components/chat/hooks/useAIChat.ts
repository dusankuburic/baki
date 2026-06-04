import {useState, useCallback, useRef, useEffect, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi, flowApi} from '@/api'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import type {ChatMessage, ContextPreview, SourceFileInfo} from '@/types/domain'

interface UseAIChatOptions {
  selectedModel: string
}

const EMPTY_ARRAY: any[] = []
const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

export function useAIChat({selectedModel}: UseAIChatOptions) {
  const doc = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)

  const provider = useChatStore(s => s.selectedProvider)
  const aiSettings = useSettingsStore(s => s.settings.ai)

  const streamingText = useChatStore(s => s.streamingText)
  const streamingMessageId = useChatStore(s => s.streamingMessageId)
  const activeStreamId = useChatStore(s => s.activeStreamId)
  const getMessages = useChatStore(s => s.getMessages)
  const appendMessage = useChatStore(s => s.appendMessage)
  const removeMessage = useChatStore(s => s.removeMessage)
  const clearThreadMessages = useChatStore(s => s.clearThreadMessages)
  const compactThread = useChatStore(s => s.compactThread)
  const updateThread = useChatStore(s => s.updateThread)
  const createThread = useChatStore(s => s.createThread)
  const switchThread = useChatStore(s => s.switchThread)
  const closeThread = useChatStore(s => s.closeThread)
  const updateStreamingMessage = useChatStore(s => s.updateStreamingMessage)
  const startStream = useChatStore(s => s.startStream)
  const endStream = useChatStore(s => s.endStream)
  const globalPendingMessage = useChatStore(s => s.pendingMessage)
  const setGlobalPendingMessage = useChatStore(s => s.setPendingMessage)

  const threads = useChatStore(s => s.threads)
  const activeThreadId = useChatStore(s => s.activeThreadId)
  const activeThreadMessages = useChatStore(s =>
    s.activeThreadId ? (s.conversations.get(s.activeThreadId) ?? EMPTY_ARRAY) : EMPTY_ARRAY
  )

  const flowThreads = useMemo(
    () => doc ? threads.filter(t => t.flowId === doc.id) : EMPTY_ARRAY as any[],
    [doc?.id, threads],
  )
  const activeThread = useMemo(
    () => threads.find(t => t.id === activeThreadId) ?? null,
    [threads, activeThreadId],
  )

  const [isThinking, setIsThinking] = useState(false)
  const [streamingTokens, setStreamingTokens] = useState(0)
  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingMessage, setPendingMessage] = useState<string | null>(null)
  const [sourceFiles, setSourceFiles] = useState<SourceFileInfo[]>(EMPTY_SOURCE_FILES)

  const isStreaming = activeStreamId !== null

  // Stable refs for use inside callbacks
  const providerRef = useRef(provider)
  providerRef.current = provider
  const streamingRef = useRef(streamingText)
  streamingRef.current = streamingText
  const docRef = useRef(doc)
  docRef.current = doc
  const streamingThreadIdRef = useRef<string | null>(null)
  const streamingMessageIdRef = useRef(streamingMessageId)
  streamingMessageIdRef.current = streamingMessageId
  const selectedModelRef = useRef(selectedModel)
  selectedModelRef.current = selectedModel

  // Accumulated text to avoid stale state in rapid updates
  const accumulatedTextRef = useRef('')
  // Track last update time for throttling streaming state updates
  const lastUpdateRef = useRef<number | null>(null)

  const isCurrentThreadStreaming = isStreaming && streamingThreadIdRef.current === activeThreadId
  const showThinking = isCurrentThreadStreaming && isThinking && !streamingText

  useEffect(() => {
    if (!doc) {
      setSourceFiles(EMPTY_SOURCE_FILES)
      return
    }
    flowApi.getSourceFiles().then((files: any) => {
      const list: SourceFileInfo[] = (files || []).map((f: any) => ({
        filename: f.filename || '',
        subflowId: f.subflowId || '',
        subflowName: f.subflowName || '',
        blockCount: f.blockCount || 0,
        lineCount: f.lineCount || 0,
      }))
      setSourceFiles(list)
    }).catch(() => {})
  }, [doc?.id])

  useEffect(() => {
    if (!doc) return
    if (flowThreads.length === 0) {
      const id = createThread(doc.id)
      if (sourceFiles.length > 0) {
        updateThread(id, {selectedSourceFiles: sourceFiles.map(f => f.filename)})
      }
      // createThread pre-populates conversations with [], which prevents the
      // load effect from running. Restore the persisted flow conversation here.
      chatApi.getConversation(doc.id, 'flow').then((conv: any) => {
        if (conv?.messages?.length > 0) {
          for (const m of conv.messages) {
            appendMessage(id, m as ChatMessage)
          }
        }
      }).catch(() => {})
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [doc?.id])

  useEffect(() => {
    if (!activeThreadId || !doc?.id) return
    // Only load from backend if the thread has no messages yet in the store
    const existing = useChatStore.getState().conversations.get(activeThreadId)
    if (existing !== undefined) return
    const thread = useChatStore.getState().threads.find(t => t.id === activeThreadId)
    const scope = thread?.contextBlockId || 'flow'
    let cancelled = false
    chatApi.getConversation(doc.id, scope).then((conv: any) => {
      if (cancelled) return
      if (conv?.messages) {
        for (const m of conv.messages) {
          appendMessage(activeThreadId, m as ChatMessage)
        }
      }
    }).catch(() => {})
    return () => { cancelled = true }
  }, [activeThreadId, appendMessage, doc?.id])

  const onChunk = useCallback((text: string) => {
    setIsThinking(prev => prev ? false : prev)
    accumulatedTextRef.current += text

    // Throttle state updates to prevent excessive re-renders (update every 80ms max)
    const now = Date.now()
    if (!lastUpdateRef.current) {
      lastUpdateRef.current = now
      updateStreamingMessage(accumulatedTextRef.current)
      setStreamingTokens(Math.ceil(accumulatedTextRef.current.length / 4))
    } else if (now - lastUpdateRef.current > 80) {
      lastUpdateRef.current = now
      updateStreamingMessage(accumulatedTextRef.current)
      setStreamingTokens(Math.ceil(accumulatedTextRef.current.length / 4))
    }
  }, [updateStreamingMessage])

  const onDone = useCallback((tokensOut: number, tokensIn: number) => {
    const content = accumulatedTextRef.current
    const curThreadId = streamingThreadIdRef.current
    const curDoc = docRef.current

    // Final update to ensure complete text is displayed
    updateStreamingMessage(content)
    lastUpdateRef.current = null

    const msg: ChatMessage = {
      id: streamingMessageIdRef.current || crypto.randomUUID(),
      role: 'assistant',
      content,
      timestamp: new Date().toISOString(),
      provider: providerRef.current,
      model: selectedModelRef.current,
      tokensIn,
      tokensOut,
      finishReason: 'stop',
    }
    if (curDoc && curThreadId) {
      appendMessage(curThreadId, msg)
      const thread = useChatStore.getState().threads.find(t => t.id === curThreadId)
      chatApi.saveConversation(curDoc.id, thread?.contextBlockId || 'flow', getMessages(curThreadId) as any).catch(() => {})
      updateThread(curThreadId, {
        tokensIn: (useChatStore.getState().threads.find(t => t.id === curThreadId)?.tokensIn ?? 0) + tokensIn,
        tokensOut: (useChatStore.getState().threads.find(t => t.id === curThreadId)?.tokensOut ?? 0) + tokensOut,
      })
    }
    streamingThreadIdRef.current = null
    accumulatedTextRef.current = ''
    endStream()
    setIsThinking(false)
    setStreamingTokens(0)
  }, [appendMessage, getMessages, endStream, updateThread])

  const onError = useCallback((error: string) => {
    const content = streamingRef.current || ''
    const curThreadId = streamingThreadIdRef.current
    const curDoc = docRef.current
    const displayContent = content
      ? content + '\n\n---\n*Error: ' + error + '*'
      : '*Error: ' + error + '*'
    const msg: ChatMessage = {
      id: streamingMessageIdRef.current || crypto.randomUUID(),
      role: 'assistant',
      content: displayContent,
      timestamp: new Date().toISOString(),
      provider: providerRef.current,
      model: selectedModelRef.current,
      finishReason: 'error',
    }
    if (curDoc && curThreadId) appendMessage(curThreadId, msg)
    streamingThreadIdRef.current = null
    endStream()
    setIsThinking(false)
    setStreamingTokens(0)
  }, [appendMessage, endStream])

  const handler = useMemo(() => ({
    onChunk,
    onDone,
    onError
  }), [onChunk, onDone, onError])

  const {registerStream} = useStreamingMessage(handler)

  const buildRequest = useCallback((text: string, overrideFiles?: string[], excludeContext?: boolean) => {
    if (!doc || !activeThread) return null
    const history = getMessages(activeThread.id)
    const providerConfig = aiSettings.providers[provider as keyof typeof aiSettings.providers]

    // Always use the latest state from the store to avoid stale closures
    const currentThread = useChatStore.getState().threads.find(t => t.id === activeThread.id)
    let filesToUse = currentThread?.selectedSourceFiles || []

    if (overrideFiles !== undefined && overrideFiles.length > 0) {
      filesToUse = overrideFiles
    }

    return {
      flowId: doc.id,
      provider,
      model: selectedModel || providerConfig?.defaultModel || '',
      messages: history.map((m: ChatMessage) => ({id: m.id, role: m.role, content: m.content, timestamp: m.timestamp})),
      userMessage: text,
      contextBlockId: excludeContext ? undefined : (activeThread.contextBlockId || ''),
      selectedSourceFiles: excludeContext ? undefined : (filesToUse.length > 0 ? filesToUse : undefined),
      temperature: providerConfig?.temperature ?? 0.3,
      maxTokens: providerConfig?.maxTokens ?? 4096,
      excludeContext: excludeContext ?? false,
    }
  }, [doc, activeThread, provider, selectedModel, aiSettings, getMessages])

  const executeSend = useCallback(async (text: string, overrideFiles?: string[], excludeContext?: boolean) => {
    if (!doc || !activeThread) return
    if (useChatStore.getState().activeStreamId !== null) return
    const req: any = buildRequest(text, overrideFiles, excludeContext)
    if (!req) return

    const userMsg: ChatMessage = {
      id: crypto.randomUUID(),
      role: 'user',
      content: text,
      timestamp: new Date().toISOString(),
      contextBlockId: activeThread.contextBlockId ?? undefined,
    }
    appendMessage(activeThread.id, userMsg)
    chatApi.saveConversation(doc.id, activeThread.contextBlockId || 'flow', getMessages(activeThread.id) as any).catch(() => {})

    const msgId = crypto.randomUUID()
    streamingThreadIdRef.current = activeThread.id
    lastUpdateRef.current = null // Reset throttle timer
    startStream('pending', msgId)
    setIsThinking(true)
    setStreamingTokens(0)

    try {
      const sid = await chatApi.streamChatMessage(req)
      if (!sid) {
        streamingThreadIdRef.current = null
        endStream()
        setIsThinking(false)
        appendMessage(activeThread.id, {
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
      if (useChatStore.getState().activeStreamId === null) {
        chatApi.cancelStream(sid).catch(() => {})
        streamingThreadIdRef.current = null
        setIsThinking(false)
        return
      }
      startStream(sid, msgId)
      registerStream(sid)
    } catch (e: any) {
      const errMsg = e?.message || String(e) || 'Failed to send message'
      appendMessage(activeThread.id, {
        id: crypto.randomUUID(),
        role: 'assistant',
        content: '*Error: ' + errMsg + '*',
        timestamp: new Date().toISOString(),
        provider,
        model: selectedModel,
        finishReason: 'error',
      } as ChatMessage)
      streamingThreadIdRef.current = null
      endStream()
      setIsThinking(false)
    }
  }, [doc, activeThread, buildRequest, appendMessage, getMessages, startStream, endStream, registerStream, provider, selectedModel])

  const handleSend = useCallback((text: string, files: string[], excludeContext?: boolean) => {
    if (files.length > 0 && activeThreadId) {
       updateThread(activeThreadId, {selectedSourceFiles: files})
    }
    executeSend(text, files.length > 0 ? files : undefined, excludeContext)
  }, [executeSend, activeThreadId, updateThread])

  // Stable refs for the effect to avoid re-triggering it when they change
  const executeSendRef = useRef(executeSend)
  executeSendRef.current = executeSend
  const updateThreadRef = useRef(updateThread)
  updateThreadRef.current = updateThread

  // React to global pending messages (e.g. from context menus)
  useEffect(() => {
    if (globalPendingMessage && doc && activeThreadId) {
      const {text, contextBlockId} = globalPendingMessage
      if (contextBlockId) {
        updateThreadRef.current(activeThreadId, {contextBlockId})
      }
      executeSendRef.current(text)
      setGlobalPendingMessage(null)
    }
  }, [globalPendingMessage, doc?.id, activeThreadId, setGlobalPendingMessage])

  const handlePreviewContext = useCallback(async (text: string, files: string[], excludeContext?: boolean) => {
    if (files.length > 0 && activeThreadId) {
       updateThread(activeThreadId, {selectedSourceFiles: files})
    }
    const req = buildRequest(text, files.length > 0 ? files : undefined, excludeContext)
    if (!req) return
    try {
      const preview = await chatApi.previewContext(req as any) as any as ContextPreview
      setContextPreview(preview)
      setPendingMessage(text)
    } catch {
      executeSend(text, files.length > 0 ? files : undefined, excludeContext)
    }
  }, [buildRequest, executeSend, activeThreadId, updateThread])

  const handleResend = useCallback(() => {
    if (!doc || !activeThread || isStreaming) return
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
    if (lastUserContent) executeSend(lastUserContent)
  }, [doc, activeThread, isStreaming, getMessages, removeMessage, executeSend])

  const handleExport = useCallback(async () => {
    if (!doc || !activeThread) return
    await chatApi.exportConversation(doc.id, activeThread.id)
  }, [doc, activeThread])

  const handleCreateThread = useCallback(() => {
    if (!doc) return
    const id = createThread(doc.id)
    if (sourceFiles.length > 0) {
      updateThread(id, {selectedSourceFiles: sourceFiles.map(f => f.filename)})
    }
  }, [doc, createThread, sourceFiles, updateThread])

  const handleCloseThread = useCallback((threadId: string) => {
    closeThread(threadId)
  }, [closeThread])

  const handleRenameThread = useCallback((threadId: string, title: string) => {
    updateThread(threadId, {title})
  }, [updateThread])

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

  const setThreadContextBlock = useCallback((blockId: string | null) => {
    if (activeThreadId) updateThread(activeThreadId, {contextBlockId: blockId})
  }, [activeThreadId, updateThread])

  const setThreadSourceFiles = useCallback((files: string[]) => {
    if (activeThreadId) updateThread(activeThreadId, {selectedSourceFiles: files})
  }, [activeThreadId, updateThread])

  const handleCancelStream = useCallback(() => {
    if (activeStreamId) {
      chatApi.cancelStream(activeStreamId).catch(() => {})
      endStream()
    }
    streamingThreadIdRef.current = null
    setIsThinking(false)
    setStreamingTokens(0)
  }, [activeStreamId, endStream])

  return {
    // state
    doc,
    selectedBlockId,
    activeThreadId,
    activeThread,
    activeThreadMessages,
    flowThreads,
    isStreaming,
    isCurrentThreadStreaming,
    showThinking,
    streamingText,
    streamingMessageId,
    isThinking,
    streamingTokens,
    sourceFiles,
    contextPreview,
    pendingMessage,
    // thread actions
    switchThread,
    // handlers
    handleSend,
    handlePreviewContext,
    handleResend,
    handleExport,
    handleCreateThread,
    handleCloseThread,
    handleRenameThread,
    handleClearContext,
    handleCompact,
    setThreadContextBlock,
    setThreadSourceFiles,
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
