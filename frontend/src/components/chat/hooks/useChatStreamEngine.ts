import {useCallback, useRef, useEffect, useMemo} from 'react'
import {chatApi} from '@/api'
import {useChatStore} from '@/stores/chatStore'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {logger} from '@/lib/logger'
import {utf8ByteLength} from '@/lib/utf8'
import type {ChatMessage, FlowDocument, ProviderID} from '@/types'

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

interface UseChatStreamEngineOptions {
  doc: FlowDocument | null
  provider: ProviderID
  selectedModel: string
  getMessages: (threadId: string) => ChatMessage[]
}

// useChatStreamEngine owns the per-stream text accumulator, the SSE handler
// wiring (via useStreamingMessage), and the per-thread generation tokens used
// to detect and discard stale sends. Extracted from useAIChat so the
// streaming machinery — the part most prone to subtle bugs (races between
// concurrent streams, stale closures in long-lived SSE callbacks) — is
// isolated from request-building and send-orchestration concerns.
export function useChatStreamEngine({doc, provider, selectedModel, getMessages}: UseChatStreamEngineOptions) {
  const appendMessage = useChatStore(s => s.appendMessage)
  const updateThread = useChatStore(s => s.updateThread)
  const updateStream = useChatStore(s => s.updateStream)
  const setStreamMeta = useChatStore(s => s.setStreamMeta)
  const endStream = useChatStore(s => s.endStream)

  // Refs mirror the latest doc/provider/selectedModel so the long-lived SSE
  // handlers below (onDone/onError, registered once via useStreamingMessage)
  // always see current values without needing to be in their dependency
  // arrays — re-subscribing the SSE listener on every prop change would drop
  // in-flight streams.
  const docRef = useRef(doc)
  useEffect(() => { docRef.current = doc })
  const providerRef = useRef(provider)
  useEffect(() => { providerRef.current = provider })
  const selectedModelRef = useRef(selectedModel)
  useEffect(() => { selectedModelRef.current = selectedModel })

  const streamAccRef = useRef(new Map<string, StreamAcc>())
  const teardownRef = useRef<(streamId: string) => void>(() => {})
  const cancelRef = useRef<(streamId: string) => void>(() => {})

  // threadGenRef is a per-thread generation token. Each send for a thread
  // bumps its token; a stale send (user cancelled, closed the thread, or sent
  // again in the same thread) detects it's no longer current after an await
  // and self-cancels. Per-thread is required since several threads can stream
  // concurrently.
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
  // the store slot or tear down SSE (the caller does cancelStream/endStream).
  const dropAcc = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    if (acc.raf !== null) cancelAnimationFrame(acc.raf)
    streamAccRef.current.delete(streamId)
  }, [])

  // beginAcc primes the accumulator for a stream that's about to start, so
  // the first chunk event finds its key.
  const beginAcc = useCallback((streamId: string, threadId: string) => {
    streamAccRef.current.set(streamId, {threadId, text: '', raf: null, first: false})
  }, [])

  // commitAssistantMessage appends the streamed text as a permanent assistant
  // message and persists the thread. Shared by the natural-completion path
  // (onDone) and the user-stop path (stopAndCommit) so an interrupted answer
  // is kept, not discarded. Persisted messages are filtered of the transient
  // error bubbles so reloaded history isn't littered with them.
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

  const cancelStream = useCallback((streamId: string) => cancelRef.current(streamId), [])

  // stopAndCommit cancels a stream the user stopped mid-generation, keeping
  // whatever text was already generated as a permanent 'interrupted' message
  // instead of discarding it.
  const stopAndCommit = useCallback((streamId: string, threadId: string, messageId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (acc) {
      if (acc.raf !== null) cancelAnimationFrame(acc.raf)
      streamAccRef.current.delete(streamId)
      if (acc.text) {
        commitAssistantMessage(threadId, messageId, acc.text, {finishReason: 'interrupted'})
      }
    }
    cancelStream(streamId)
  }, [commitAssistantMessage, cancelStream])

  return {
    registerStream,
    cancelStream,
    beginAcc,
    dropAcc,
    stopAndCommit,
    commitAssistantMessage,
    bumpGen,
    isCurrentGen,
  }
}
