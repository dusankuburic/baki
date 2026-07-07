import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {utf8ByteLength} from '@/lib/utf8'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState, getEventConnectionState} from '@/api/client'

// How long registerStream waits for the SSE connection to reach 'open' before
// giving up. Covers a token refresh plus the first few reconnect backoff steps
// (1s/2s/4s); past that the backend is genuinely unreachable and the user
// needs an error instead of a stuck thinking indicator.
const OPEN_WAIT_TIMEOUT_MS = 15_000

// Stall probe: while a stream is registered, if no chunk/tool/terminal event
// arrives for this long, fetch the authoritative state via resume. The healthy
// path emits every ≤16ms (chunkCoalescer), and the backend's 90s provider-idle
// watchdog stores an error that resume returns — so a stalled stream surfaces
// within one-to-two probes. This is the only recovery trigger for a dropped
// done/error event (the chunk-count mismatch check needs `done` to arrive) and
// for a backend worker that died without emitting anything.
const STALL_PROBE_MS = 30_000
// Consecutive no-progress probes before giving up and erroring the slot —
// covers a stream the backend lost with neither done nor error recorded.
const STALL_PROBE_LIMIT = 3

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
  stallTimer: ReturnType<typeof setTimeout> | null
  probeMisses: number
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

  // armStall schedules the next stall probe via a ref because armStall and
  // probeStall reference each other (the probe re-arms on no-progress).
  const probeStallRef = useRef<(streamId: string) => void>(() => {})
  const armStall = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (!sub) return
    if (sub.stallTimer) clearTimeout(sub.stallTimer)
    sub.stallTimer = setTimeout(() => probeStallRef.current(streamId), STALL_PROBE_MS)
  }, [])

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
      } else {
        // Still generating after a reconnect-resume — keep watching for stalls.
        armStall(streamId)
      }
    }).catch(() => {
      // Stream no longer available. When this was a done-verification pass we
      // still have to finish with what accumulated; reconnect probes just drop.
      if (!subsRef.current.has(streamId)) return
      if (finish) handlerRef.current.onDone(finish.tokensOut, finish.tokensIn, streamId)
    })
  }, [armStall])

  // teardownStream clears one stream's listener after it completes naturally
  // (onDone/onError). Unlike cancel, it does NOT cancel on the backend.
  const teardownStream = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (sub) {
      if (sub.stallTimer) clearTimeout(sub.stallTimer)
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

  // probeStall fires when a registered stream saw no event for STALL_PROBE_MS.
  // The backend's terminal event may have been dropped (saturated SSE buffer)
  // or never emitted (worker died) — resume returns the authoritative buffered
  // state, so finish/error from that instead of spinning forever.
  const probeStall = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (!sub) return
    sub.stallTimer = null
    if (getEventConnectionState() !== 'open') {
      // Disconnected: the reconnect listener owns recovery (delta-resume once
      // the connection reopens); keep watching until it does.
      armStall(streamId)
      return
    }
    // Live chunks may have been dropped, so fetch the authoritative full
    // buffer (same reasoning as the chunk-count mismatch path).
    sub.countsInvalid = true
    const before = handlerRef.current.getAccLength?.(streamId) ?? 0
    const miss = () => {
      if (++sub.probeMisses >= STALL_PROBE_LIMIT) {
        handlerRef.current.onError('The response stalled — the connection to the backend may have been lost.', streamId)
      } else {
        armStall(streamId)
      }
    }
    chatApi.resumeStream(streamId, 0).then(res => {
      if (!subsRef.current.has(streamId)) return
      if (res.error) {
        handlerRef.current.onError(res.error, streamId)
        return
      }
      if (res.text) handlerRef.current.onReplace(res.text, streamId)
      if (res.done) {
        handlerRef.current.onDone(res.tokensOut || 0, res.tokensIn || 0, streamId)
        return
      }
      if (res.text && utf8ByteLength(res.text) > before) {
        // Still generating, just slowly (or the live chunks were dropped).
        sub.probeMisses = 0
        armStall(streamId)
      } else {
        miss()
      }
    }).catch(() => {
      // Stream unknown (never created / retention expired) or the probe
      // request itself failed. Count it as a miss so a transient failure
      // doesn't kill a live stream, but a truly lost one errors out.
      if (!subsRef.current.has(streamId)) return
      miss()
    })
  }, [armStall])

  useEffect(() => { probeStallRef.current = probeStall }, [probeStall])

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

    const sub: StreamSub = {unsub: () => {}, received: 0, countsInvalid: false, stallTimer: null, probeMisses: 0}

    const unsub = await subscribeToEvents((event: { name: string; data: unknown }) => {
      if (event.name !== 'chat:event') return
      const payload = event.data as Record<string, unknown> | null
      if (!payload || payload.streamId !== streamId) return

      const type = payload.type as string
      const data = (payload.data as Record<string, unknown>) || {}

      switch (type) {
        case 'chunk':
          sub.received++
          sub.probeMisses = 0
          armStall(streamId)
          handlerRef.current.onChunk((data.content as string) || '', streamId)
          break
        case 'done': {
          if (sub.stallTimer) { clearTimeout(sub.stallTimer); sub.stallTimer = null }
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
          if (sub.stallTimer) { clearTimeout(sub.stallTimer); sub.stallTimer = null }
          handlerRef.current.onError((data.message as string) || 'Unknown error', streamId)
          break
        case 'tool':
          sub.probeMisses = 0
          armStall(streamId)
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
    // Watch from registration: a backend that never emits anything (worker
    // panic, stream never created) is caught by the first stall probe.
    armStall(streamId)

    // Wait for the SSE connection to be fully 'open' before signaling the backend
    // to begin emitting chunks. If beginStream is called while the connection is
    // still establishing (connecting/reconnecting), the backend's EventManager
    // has no client for this user yet and the initial chunks will be dropped.
    // Bounded: past OPEN_WAIT_TIMEOUT_MS the backend is unreachable, so reject
    // and let the caller surface an error instead of pinning the thinking
    // indicator forever (connectEvents itself retries with backoff forever).
    if (getEventConnectionState() !== 'open') {
      await new Promise<void>((resolve, reject) => {
        let unsubConn: (() => void) | null = null
        let settled = false
        const settle = (err?: Error) => {
          if (settled) return
          settled = true
          clearTimeout(timer)
          unsubConn?.()
          if (err) reject(err); else resolve()
        }
        const timer = setTimeout(() => {
          if (isCanceledRef.current) {
            settle()
            return
          }
          // Drop this stream's registration before failing, so the reconnect
          // listener doesn't keep resuming a dead entry.
          teardownStream(streamId)
          settle(new Error('Could not connect to the event stream — the backend may still be starting. Please try again.'))
        }, OPEN_WAIT_TIMEOUT_MS)
        const check = (state: EventConnectionState) => {
          if (state === 'open' || isCanceledRef.current) settle()
        }
        unsubConn = subscribeConnectionState(check)
        // subscribeConnectionState invokes check synchronously; if that
        // settled us, the returned unsubscriber wasn't in scope yet.
        if (settled) unsubConn()
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
  }, [resumeInto, armStall, teardownStream])

  useEffect(() => {
    isCanceledRef.current = false
    const subs = subsRef.current
    return () => {
      isCanceledRef.current = true
      for (const [sid, s] of subs) {
        if (s.stallTimer) clearTimeout(s.stallTimer)
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
