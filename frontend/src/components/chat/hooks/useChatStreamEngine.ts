import {useCallback, useEffect, useMemo, useRef} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useOptionalToast} from '@/components/shared/Toast'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {useStreamAccumulators} from './stream-engine/accumulator'
import {useCommit} from './stream-engine/commit'
import {useStreamEvents} from './stream-engine/events'
import {useFixEvents} from './stream-engine/fixes'
import {useTurnLifecycle} from './stream-engine/lifecycle'
import type {StreamAcc, ToastApi} from './stream-engine/types'

export type {StreamMeta, SendOptions, TurnModel} from './stream-engine/types'

export function useChatStreamEngine() {
  const toast = useOptionalToast()
  const toastRef = useRef<ToastApi | null>(toast)
  useEffect(() => {
    toastRef.current = toast
  }, [toast])

  const appendMessage = useChatStore(s => s.appendMessage)
  const updateThread = useChatStore(s => s.updateThread)
  const updateStream = useChatStore(s => s.updateStream)
  const setStreamMeta = useChatStore(s => s.setStreamMeta)
  const addToolCall = useChatStore(s => s.addToolCall)
  const setFixProposal = useChatStore(s => s.setFixProposal)
  const patchFixProposal = useChatStore(s => s.patchFixProposal)
  const replaceStreamTools = useChatStore(s => s.replaceStreamTools)
  const replaceStreamFixes = useChatStore(s => s.replaceStreamFixes)
  const startStream = useChatStore(s => s.startStream)
  const endStream = useChatStore(s => s.endStream)
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)

  const streamAccRef = useRef(new Map<string, StreamAcc>())
  const teardownRef = useRef<(streamId: string) => void>(() => {})
  const cancelRef = useRef<(streamId: string) => void>(() => {})
  const autoResendRef = useRef<(streamId: string) => string | null>(() => null)

  const {onChunk, onReplace, onToolStatus, onToolResult, takeAcc, dropAcc, beginAcc, onAppend, getAccLength} =
    useStreamAccumulators({streamAccRef, teardownRef, updateStream, setStreamMeta, addToolCall})

  const {commitAssistantMessage, bumpThreadTokens} = useCommit({toastRef, appendMessage, updateThread})

  const {notifyBudget, onDone, onError} = useStreamEvents({
    toastRef,
    takeAcc,
    commitAssistantMessage,
    bumpThreadTokens,
    endStream,
    bumpProviderEpoch,
    appendMessage,
    autoResendRef,
  })

  const {onFixProposal, onFixDecision, respondFixProposal, onResumeState} = useFixEvents({
    streamAccRef,
    setFixProposal,
    patchFixProposal,
    replaceStreamTools,
    replaceStreamFixes,
  })

  const handler = useMemo(
    () => ({
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
    }),
    [
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
    ],
  )

  const {registerStream, confirmStream, cancel, teardownStream, resumeInto} = useStreamingMessage(handler)
  useEffect(() => {
    teardownRef.current = teardownStream
  }, [teardownStream])
  useEffect(() => {
    cancelRef.current = cancel
  }, [cancel])

  const cancelStream = useCallback((streamId: string) => cancelRef.current(streamId), [])

  const {bumpGen, isCurrentGen, stopAndCommit, send, resumeAfterReload, stop, abandon} = useTurnLifecycle({
    toastRef,
    streamAccRef,
    teardownRef,
    autoResendRef,
    registerStream,
    confirmStream,
    resumeInto,
    cancelStream,
    dropAcc,
    beginAcc,
    commitAssistantMessage,
    notifyBudget,
    startStream,
    endStream,
    updateStream,
    appendMessage,
    updateThread,
  })

  useEffect(
    () =>
      useChatStore.subscribe((state, prev) => {
        if (state.streams === prev.streams) return
        for (const [sid, acc] of streamAccRef.current) {
          const slot = state.streams[acc.threadId]
          if (slot && slot.streamId === sid) {
            acc.linked = true
            continue
          }
          if (!acc.linked) continue
          bumpGen(acc.threadId)
          dropAcc(sid)
          cancelStream(sid)
        }
      }),
    [bumpGen, cancelStream, dropAcc],
  )

  useEffect(() => {
    const accs = streamAccRef.current
    return () => {
      const threads = new Set<string>()
      for (const acc of accs.values()) {
        if (acc.raf !== null) cancelAnimationFrame(acc.raf)
        threads.add(acc.threadId)
      }
      accs.clear()
      for (const tid of threads) useChatStore.getState().endStream(tid)
    }
  }, [])

  return useMemo(
    () => ({
      send,
      stop,
      abandon,
      respondFixProposal,
      resumeAfterReload,
      registerStream,
      cancelStream,
      beginAcc,
      dropAcc,
      stopAndCommit,
      commitAssistantMessage,
      bumpGen,
      isCurrentGen,
    }),
    [
      send,
      stop,
      abandon,
      respondFixProposal,
      resumeAfterReload,
      registerStream,
      cancelStream,
      beginAcc,
      dropAcc,
      stopAndCommit,
      commitAssistantMessage,
      bumpGen,
      isCurrentGen,
    ],
  )
}

export type ChatStreamEngine = ReturnType<typeof useChatStreamEngine>
