import {useState, useCallback, useRef, useEffect, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi} from '@/api'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {logger} from '@/lib/logger'
import {useChatConversations} from './useChatConversations'
import {useChatThreads} from './useChatThreads'
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

  const streamingText = useChatStore(s => s.streamingText)
  const streamingMessageId = useChatStore(s => s.streamingMessageId)
  const activeStreamId = useChatStore(s => s.activeStreamId)
  const getMessages = useChatStore(s => s.getMessages)
  const appendMessage = useChatStore(s => s.appendMessage)
  const removeMessage = useChatStore(s => s.removeMessage)
  const updateThread = useChatStore(s => s.updateThread)
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
    () => doc ? threads.filter(t => t.flowId === doc.id) : [],
    [doc, threads],
  )
  const activeThread = useMemo(
    () => threads.find(t => t.id === activeThreadId) ?? null,
    [threads, activeThreadId],
  )

  const {sourceFiles} = useChatConversations({doc, flowThreads, activeThreadId})
  const threadActions = useChatThreads({doc, activeThreadId, sourceFiles})

  const [isThinking, setIsThinking] = useState(false)
  const [streamingTokens, setStreamingTokens] = useState(0)
  const [contextPreview, setContextPreview] = useState<ContextPreview | null>(null)
  const [pendingMessage, setPendingMessage] = useState<string | null>(null)
  const [toolStatus, setToolStatus] = useState<string | null>(null)

  const isStreaming = activeStreamId !== null

  const providerRef = useRef(provider)
  useEffect(() => { providerRef.current = provider })
  const streamingRef = useRef(streamingText)
  useEffect(() => { streamingRef.current = streamingText })
  const docRef = useRef(doc)
  useEffect(() => { docRef.current = doc })
  const streamingThreadIdRef = useRef<string | null>(null)
  const [streamingThreadId, setStreamingThreadIdState] = useState<string | null>(null)
  const setStreamingThreadId = useCallback((id: string | null) => {
    streamingThreadIdRef.current = id
    setStreamingThreadIdState(id)
  }, [])
  const streamingMessageIdRef = useRef(streamingMessageId)
  useEffect(() => { streamingMessageIdRef.current = streamingMessageId })
  const selectedModelRef = useRef(selectedModel)
  useEffect(() => { selectedModelRef.current = selectedModel })

  const accumulatedTextRef = useRef('')
  const lastUpdateRef = useRef<number | null>(null)

  const isCurrentThreadStreaming = isStreaming && streamingThreadId === activeThreadId
  const showThinking = isCurrentThreadStreaming && isThinking && !streamingText

  const onChunk = useCallback((text: string) => {
    setIsThinking(prev => prev ? false : prev)
    accumulatedTextRef.current += text
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

  const onReplace = useCallback((text: string) => {
    accumulatedTextRef.current = text
    updateStreamingMessage(text)
    setStreamingTokens(Math.ceil(text.length / 4))
  }, [updateStreamingMessage])

  const onToolStatus = useCallback((label: string) => {
    setIsThinking(false)
    setToolStatus(label)
  }, [])

  const onDone = useCallback((tokensOut: number, tokensIn: number) => {
    const content = accumulatedTextRef.current
    const curThreadId = streamingThreadIdRef.current
    const curDoc = docRef.current

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
      chatApi.saveConversation(curDoc.id, thread?.contextBlockId || 'flow', getMessages(curThreadId) as ChatMessage[]).catch((err) => { logger.warn('Failed to save conversation', err) })
      updateThread(curThreadId, {
        tokensIn: (useChatStore.getState().threads.find(t => t.id === curThreadId)?.tokensIn ?? 0) + tokensIn,
        tokensOut: (useChatStore.getState().threads.find(t => t.id === curThreadId)?.tokensOut ?? 0) + tokensOut,
      })
    }
    setStreamingThreadId(null)
    accumulatedTextRef.current = ''
    endStream()
    setIsThinking(false)
    setStreamingTokens(0)
    setToolStatus(null)
  }, [appendMessage, getMessages, endStream, updateThread, updateStreamingMessage, setStreamingThreadId])

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
    setStreamingThreadId(null)
    accumulatedTextRef.current = ''
    lastUpdateRef.current = null
    endStream()
    setIsThinking(false)
    setStreamingTokens(0)
    setToolStatus(null)
  }, [appendMessage, endStream, setStreamingThreadId])

  const handler = useMemo(() => ({
    onChunk, onReplace, onDone, onError, onToolStatus,
  }), [onChunk, onReplace, onDone, onError, onToolStatus])

  const {registerStream} = useStreamingMessage(handler)

  const buildRequest = useCallback((text: string, overrideFiles?: string[], excludeContext?: boolean) => {
    if (!doc || !activeThread) return null
    const history = getMessages(activeThread.id)
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
      messages: history.map((m: ChatMessage) => ({id: m.id, role: m.role, content: m.content, timestamp: m.timestamp})),
      userMessage: text,
      contextBlockId: excludeContext ? undefined : (activeThread.contextBlockId || ''),
      selectedSourceFiles: excludeContext ? undefined : (filesToUse.length > 0 ? filesToUse : undefined),
      temperature: providerConfig?.temperature ?? 0.3,
      maxTokens: providerConfig?.maxTokens ?? 4096,
      excludeContext: excludeContext ?? false,
      useTools: currentThread?.useTools ?? false,
    }
  }, [doc, activeThread, provider, selectedModel, aiSettings, getMessages])

  const executeSend = useCallback(async (text: string, overrideFiles?: string[], excludeContext?: boolean) => {
    if (!doc || !activeThread) return
    if (useChatStore.getState().activeStreamId !== null) return
    const req = buildRequest(text, overrideFiles, excludeContext) as ChatRequest | null
    if (!req) return

    const userMsg: ChatMessage = {
      id: crypto.randomUUID(),
      role: 'user',
      content: text,
      timestamp: new Date().toISOString(),
      contextBlockId: activeThread.contextBlockId ?? undefined,
    }
    appendMessage(activeThread.id, userMsg)
    chatApi.saveConversation(doc.id, activeThread.contextBlockId || 'flow', getMessages(activeThread.id) as ChatMessage[]).catch((err) => { logger.warn('Failed to save conversation', err) })

    const msgId = crypto.randomUUID()
    setStreamingThreadId(activeThread.id)
    lastUpdateRef.current = null
    startStream('pending', msgId)
    setIsThinking(true)
    setStreamingTokens(0)
    setToolStatus(null)

    try {
      const sid = await chatApi.streamChatMessage(req)
      if (!sid) {
        setStreamingThreadId(null)
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
        chatApi.cancelStream(sid).catch((err) => { logger.warn('Failed to cancel stream', err) })
        setStreamingThreadId(null)
        setIsThinking(false)
        return
      }
      startStream(sid, msgId)
      await registerStream(sid)
    } catch (e: unknown) {
      const errMsg = e instanceof Error ? e.message : String(e) || 'Failed to send message'
      appendMessage(activeThread.id, {
        id: crypto.randomUUID(),
        role: 'assistant',
        content: '*Error: ' + errMsg + '*',
        timestamp: new Date().toISOString(),
        provider,
        model: selectedModel,
        finishReason: 'error',
      } as ChatMessage)
      setStreamingThreadId(null)
      accumulatedTextRef.current = ''
      lastUpdateRef.current = null
      endStream()
      setIsThinking(false)
      setStreamingTokens(0)
      setToolStatus(null)
    }
  }, [doc, activeThread, buildRequest, appendMessage, getMessages, startStream, endStream, registerStream, provider, selectedModel, setStreamingThreadId])

  const handleSend = useCallback((text: string, files: string[], excludeContext?: boolean) => {
    if (files.length > 0 && activeThreadId) {
       updateThread(activeThreadId, {selectedSourceFiles: files})
    }
    executeSend(text, files.length > 0 ? files : undefined, excludeContext)
  }, [executeSend, activeThreadId, updateThread])

  const executeSendRef = useRef(executeSend)
  useEffect(() => { executeSendRef.current = executeSend })
  const updateThreadRef = useRef(updateThread)
  useEffect(() => { updateThreadRef.current = updateThread })

  useEffect(() => {
    if (globalPendingMessage && doc && activeThreadId) {
      const {text, contextBlockId} = globalPendingMessage
      if (contextBlockId) {
        updateThreadRef.current(activeThreadId, {contextBlockId})
      }
      executeSendRef.current(text)
      setGlobalPendingMessage(null)
    }
  }, [globalPendingMessage, doc, activeThreadId, setGlobalPendingMessage])

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

  const handleCancelStream = useCallback(() => {
    if (activeStreamId) {
      chatApi.cancelStream(activeStreamId).catch((err) => { logger.warn('Failed to cancel stream', err) })
      endStream()
    }
    setStreamingThreadId(null)
    setIsThinking(false)
    setStreamingTokens(0)
  }, [activeStreamId, endStream, setStreamingThreadId])

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
    streamingText,
    streamingMessageId,
    isThinking,
    streamingTokens,
    sourceFiles,
    contextPreview,
    pendingMessage,
    toolStatus,
    ...threadActions,
    handleSend,
    handlePreviewContext,
    handleResend,
    handleExport,
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
