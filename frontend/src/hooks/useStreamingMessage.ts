import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState, getEventConnectionState} from '@/api/client'

// Every callback receives the streamId it was dispatched for, so handlers can
// reject events from a stale stream (a listener not yet torn down when the
// next stream starts) instead of relying purely on teardown ordering.
interface StreamHandler {
  onChunk: (text: string, streamId: string) => void
  onReplace: (text: string, streamId: string) => void
  onDone: (tokensOut: number, tokensIn: number, streamId: string) => void
  onError: (error: string, streamId: string) => void
  // onToolStatus is emitted during the read-only tool/agent loop with a short
  // label for the current step (e.g. "Searching flow"). Optional.
  onToolStatus?: (label: string, streamId: string) => void
  // onAppend adds a delta to the stream's accumulated text (delta-resume path).
  // Optional; when absent, resumeInto falls back to a full onReplace.
  onAppend?: (delta: string, streamId: string) => void
  // getAccLength returns the bytes of accumulated text the client already holds
  // for this stream, so a delta-resume can request only the tail. Optional.
  getAccLength?: (streamId: string) => number
}

// StreamSub is one registered stream's live state: its SSE unsubscriber plus
// the dropped-chunk bookkeeping. The backend's done event carries how many
// chunk events it emitted; a saturated SSE buffer silently drops events, so
// if the received count disagrees (or a reconnect made counting meaningless)
// the authoritative buffered text is fetched via resume before finishing.
interface StreamSub {
  unsub: () => void
  received: number
  countsInvalid: boolean
}

// useStreamingMessage subscribes to chat stream events. Multiple streams may
// be registered at once (one per chat thread); each has an independent
// subscription and dropped-chunk state. One shared connection-state listener
// resumes every registered stream after an SSE reconnect.
export function useStreamingMessage(handler: StreamHandler) {
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  const subsRef = useRef(new Map<string, StreamSub>())
  const unregisterConnRef = useRef<(() => void) | null>(null)
  const isCanceledRef = useRef(false)

  // resumeInto fetches a stream's buffered state and replays it through the
  // handlers — used after reconnects and on chunk-count mismatches.
  //
  // mode:
  //   - 'delta' (reconnect): the client's accumulated text is a clean prefix of
  //     the backend buffer, so request only the tail (from = getAccLength) and
  //     append via onAppend. Avoids re-fetching + re-parsing the full buffer on
  //     every reconnect (which happens at each access-token TTL expiry).
  //   - 'full' (mismatch): possible mid-stream gaps, so fetch the authoritative
  //     full buffer and onReplace.
  const resumeInto = useCallback((streamId: string, opts?: {finish?: {tokensOut: number; tokensIn: number}; mode?: 'delta' | 'full'}) => {
    const finish = opts?.finish
    const mode = opts?.mode ?? 'full'
    const useDelta = mode === 'delta' && handlerRef.current.onAppend && handlerRef.current.getAccLength
    const from = useDelta ? (handlerRef.current.getAccLength?.(streamId) ?? 0) : 0
    chatApi.resumeStream(streamId, from).then(res => {
      if (!subsRef.current.has(streamId)) return
      if (res.text) {
        if (useDelta) {
          handlerRef.current.onAppend!(res.text, streamId)
        } else {
          handlerRef.current.onReplace(res.text, streamId)
        }
      }
      if (res.error) {
        handlerRef.current.onError(res.error, streamId)
      } else if (res.done || finish) {
        handlerRef.current.onDone(res.tokensOut || finish?.tokensOut || 0, res.tokensIn || finish?.tokensIn || 0, streamId)
      }
    }).catch(() => {
      // Stream no longer available. When this was a done-verification pass we
      // still have to finish with what accumulated; reconnect probes just drop.
      if (!subsRef.current.has(streamId)) return
      if (finish) handlerRef.current.onDone(finish.tokensOut, finish.tokensIn, streamId)
    })
  }, [])

  // teardownStream clears one stream's listener after it completes naturally
  // (onDone/onError). Unlike cancel, it does NOT cancel on the backend.
  const teardownStream = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (sub) {
      sub.unsub()
      subsRef.current.delete(streamId)
    }
    if (subsRef.current.size === 0 && unregisterConnRef.current) {
      unregisterConnRef.current()
      unregisterConnRef.current = null
    }
  }, [])

  const cancel = useCallback((streamId: string) => {
    chatApi.cancelStream(streamId).catch((err) => { logger.warn('Failed to cancel stream', err) })
    teardownStream(streamId)
  }, [teardownStream])

  // registerStream subscribes the per-stream SSE listener (and the shared
  // connection-state recovery hook) and waits for the SSE connection to be
  // 'open'. When `begin` is true (legacy handshake) it also calls /chat/begin
  // to unblock backend emission and to fetch any buffered terminal state for a
  // stream that finished before this subscription existed. When `begin` is
  // false (C-1: caller supplied a clientStreamId and will POST create after
  // this returns) the begin call is skipped — the backend emits immediately on
  // stream creation because the listener is already in place.
  const registerStream = useCallback(async (streamId: string, begin = true) => {
    // One shared connection-state listener for all registered streams: after
    // the SSE connection recovers, fetch each stream's full buffered state.
    // The backend retains finished streams briefly, so even one that completed
    // while we were disconnected recovers its final text and done/error signal
    // (otherwise the UI would hang in the streaming state).
    if (!unregisterConnRef.current) {
      let wasReconnecting = false
      unregisterConnRef.current = subscribeConnectionState((state: EventConnectionState) => {
        if (state === 'reconnecting' || state === 'connecting') {
          wasReconnecting = true
        } else if (state === 'open' && wasReconnecting) {
          wasReconnecting = false
          for (const [sid, s] of subsRef.current) {
            s.countsInvalid = true // chunks were missed while disconnected
            // Delta-resume: the client's accumulated text is a clean prefix, so
            // fetch only the tail instead of re-parsing the whole buffer.
            resumeInto(sid, {mode: 'delta'})
          }
        }
      })
    }

    const sub: StreamSub = {unsub: () => {}, received: 0, countsInvalid: false}

    const unsub = await subscribeToEvents((event: { name: string; data: unknown }) => {
      if (event.name !== 'chat:event') return
      const payload = event.data as Record<string, unknown> | null
      if (!payload || payload.streamId !== streamId) return

      const type = payload.type as string
      const data = (payload.data as Record<string, unknown>) || {}

      switch (type) {
        case 'chunk':
          sub.received++
          handlerRef.current.onChunk((data.content as string) || '', streamId)
          break
        case 'done': {
          const tokensOut = (data.tokensOut as number) || 0
          const tokensIn = (data.tokensIn as number) || 0
          const expected = data.chunks
          if (sub.countsInvalid || (typeof expected === 'number' && expected !== sub.received)) {
            // Some live chunks never reached us — replace the accumulated text
            // with the server's buffer so the committed message is complete.
            resumeInto(streamId, {finish: {tokensOut, tokensIn}, mode: 'full'})
          } else {
            handlerRef.current.onDone(tokensOut, tokensIn, streamId)
          }
          break
        }
        case 'error':
          handlerRef.current.onError((data.message as string) || 'Unknown error', streamId)
          break
        case 'tool':
          handlerRef.current.onToolStatus?.((data.label as string) || (data.name as string) || 'Using tool', streamId)
          break
      }
    })
    sub.unsub = unsub

    // If the hook unmounted while the async subscription was in flight,
    // immediately unsubscribe to prevent a leak.
    if (isCanceledRef.current) {
      unsub()
      return
    }
    subsRef.current.set(streamId, sub)

    // Wait for the SSE connection to be fully 'open' before signaling the backend
    // to begin emitting chunks. If beginStream is called while the connection is
    // still establishing (connecting/reconnecting), the backend's EventManager
    // has no client for this user yet and the initial chunks will be dropped.
    if (getEventConnectionState() !== 'open') {
      await new Promise<void>((resolve) => {
        const check = (state: EventConnectionState) => {
          if (state === 'open' || isCanceledRef.current) {
            unsubConn()
            resolve()
          }
        }
        const unsubConn = subscribeConnectionState(check)
      })
    }

    if (isCanceledRef.current) return

    if (!begin) return

    // If beginStream fails the backend goroutine blocks on awaitStart() until
    // its stream-cap timeout fires — with no event emitted afterward. Propagate
    // the error so the caller (executeSend) can clean up the streaming state.
    const res = await chatApi.beginStream(streamId)

    // The stream may have already finished before we began: a fail-fast
    // pre-stream error (bad provider, budget) emitted its terminal event
    // before our SSE subscription existed, so no event will ever arrive.
    // /begin returns that buffered state — deliver it directly.
    if (res?.status === 'finished') {
      if (isCanceledRef.current || !subsRef.current.has(streamId)) return
      if (res.text) {
        handlerRef.current.onReplace(res.text, streamId)
      }
      if (res.error) {
        handlerRef.current.onError(res.error, streamId)
      } else if (res.done) {
        handlerRef.current.onDone(res.tokensOut || 0, res.tokensIn || 0, streamId)
      }
    }
  }, [resumeInto])

  useEffect(() => {
    isCanceledRef.current = false
    const subs = subsRef.current
    return () => {
      isCanceledRef.current = true
      for (const [sid, s] of subs) {
        s.unsub()
        chatApi.cancelStream(sid).catch(() => {})
      }
      subs.clear()
      if (unregisterConnRef.current) {
        unregisterConnRef.current()
        unregisterConnRef.current = null
      }
    }
  }, [])

  return {registerStream, cancel, teardownStream}
}
