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

// useChatStreamEngine owns every chat stream: sending a turn, the per-stream
// text accumulator, the SSE handler wiring (via useStreamingMessage), stop /
// abandon, and the per-thread generation tokens used to discard stale sends.
//
// It is mounted ONCE, by ChatStreamProvider, at the app shell — never by the
// chat panel. The panel mounts and unmounts (inspector collapse, mobile
// drawer, pop-out, breakpoint change); owning the streams there meant every
// unmount cancelled the replies in flight and left their threads stuck on
// "Stop". Everything the engine needs therefore comes from the stores or from
// the arguments of a send — nothing from the panel.
//
// The pieces live in ./stream-engine/: accumulator (the per-stream text
// accumulation), commit (persist + commit of finished turns), events (done /
// error handling), fixes (approval cards and the resume journal) and
// lifecycle (send / resume / stop / abandon). This hook composes them and
// wires the SSE handler.
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

  const {registerStream, cancel, teardownStream, resumeInto} = useStreamingMessage(handler)
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
    registerStream,
    resumeInto,
    cancelStream,
    dropAcc,
    beginAcc,
    commitAssistantMessage,
    notifyBudget,
    startStream,
    endStream,
    appendMessage,
    updateThread,
  })

  // A slot can disappear without going through stop/abandon: the logout
  // reset, clearFlowThreads, or a thread closed through the store directly.
  // Its accumulator must go too, and the backend stream be cancelled — else
  // the provider keeps generating (and billing) for a listener that is gone.
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
          // The stream went away from under an in-flight send (logout, the
          // thread closed): invalidate that send too, or it goes on to create
          // the backend stream for a slot that no longer exists.
          bumpGen(acc.threadId)
          dropAcc(sid)
          cancelStream(sid)
        }
      }),
    [bumpGen, cancelStream, dropAcc],
  )

  // The host unmounting (app teardown) must not leave slots claiming to
  // stream: end every thread that still has an accumulator. The backend
  // streams themselves are cancelled by useStreamingMessage's own cleanup.
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
      // Lower-level pieces, exposed for tests and the resend path.
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
