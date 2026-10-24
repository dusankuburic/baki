import {useCallback, useEffect, useMemo, useRef} from 'react'
import i18n from '@/i18n'
import {chatApi} from '@/api'
import {isReplicaUnreachable} from '@/lib/chatErrors'
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

// A stop the server could not relay to the replica running the stream: the stream keeps running there until it ends
// by itself, so say so and offer the stop again (which offers itself again while the replica stays out of reach).
function offerStopRetry(toast: ToastApi | null, streamId: string): void {
  toast?.warning(i18n.t('chat:errors.stopUnreachable'), {
    description: i18n.t('chat:errors.stopUnreachableBody'),
    action: {
      label: i18n.t('chat:errors.stopRetry'),
      onClick: () => {
        chatApi.cancelStream(streamId).catch(err => {
          if (isReplicaUnreachable(err)) offerStopRetry(toast, streamId)
        })
      },
    },
  })
}

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

  const {onChunk, onReplace, onToolStatus, onToolResult, onNotice, takeAcc, dropAcc, beginAcc, onAppend, getAccLength} =
    useStreamAccumulators({streamAccRef, teardownRef, updateStream, setStreamMeta, addToolCall})

  const {commitAssistantMessage, bumpThreadTokens} = useCommit({toastRef, appendMessage, updateThread})

  const {onDone, onError} = useStreamEvents({
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
    toastRef,
    setFixProposal,
    patchFixProposal,
    replaceStreamTools,
    replaceStreamFixes,
    setStreamMeta,
  })

  const onStopUnreachable = useCallback((streamId: string) => offerStopRetry(toastRef.current, streamId), [toastRef])

  const handler = useMemo(
    () => ({
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onNotice,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
      onStopUnreachable,
    }),
    [
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onNotice,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
      onStopUnreachable,
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
