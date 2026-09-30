// Translated through the i18next instance rather than the hook: these strings
// are produced inside long-lived streaming callbacks whose dependency arrays
// are deliberately stable, and re-creating them per language change would tear
// down live SSE subscriptions.
import i18n from '@/i18n'
import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {utf8ByteLength} from '@/lib/utf8'
import {
  parseChatEvent,
  chatEventStreamId,
  parseResumeEvents,
  FixProposalPayload,
  ToolResultPayload,
  ChatEvent,
  DoneMeta,
  ErrorUsage,
} from '@/lib/chatEvent'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState, getEventConnectionState} from '@/api/client'

// registerStream gives up waiting for SSE 'open' after this (token refresh +
// a few backoff steps); past it the user needs an error, not a stuck spinner.
const OPEN_WAIT_TIMEOUT_MS = 15_000

// No event for this long → probe the authoritative state via resume. Recovers
// dropped done/error events and dead workers; the backend's 90s provider-idle
// watchdog makes a stalled stream surface within one-to-two probes.
const STALL_PROBE_MS = 30_000
const STALL_PROBE_LIMIT = 3

// Callbacks receive the streamId they were dispatched for, so handlers can
// reject stale-stream events.
//
// EXPORTED deliberately: a test that re-declares this shape locally cannot
// detect a signature drift, and one did exactly that — its private copy carried
// a 5-argument onFixDecision that the dispatcher below never called with 5
// arguments, so a real bug sat behind a green test. Bind to this type.
export interface StreamHandler {
  onChunk: (text: string, streamId: string) => void
  onReplace: (text: string, streamId: string) => void
  // `meta` is the served model and billed rates when the backend reported
  // them (see DoneMeta); undefined for a plain done.
  onDone: (tokensOut: number, tokensIn: number, streamId: string, meta?: DoneMeta) => void
  // `code` is the failure's machine-readable class (AI_BUDGET_EXCEEDED, …)
  // when the backend sent one — see internal/service/chat_errors.go.
  // `usage` carries what a BILLED failure reports (F-15: token counts that
  // must reach the meter, plus the served model / rates as done-meta) and
  // `shared` when the failure came from the organisation's shared
  // credential (F-42). Undefined for a plain unbilled error.
  onError: (error: string, streamId: string, code?: string, usage?: ErrorUsage) => void
  onToolStatus?: (label: string, streamId: string) => void
  // One finished tool execution (transparency trail record).
  onToolResult?: (record: ToolResultPayload, streamId: string) => void
  // onResumeState receives the replayable journal from a resume response —
  // the reconnecting client rebuilds its tool trail and any pending approval
  // cards from it. Wholesale-replace semantics: call even when empty only if
  // events were present (undefined = old backend, keep live state).
  onResumeState?: (events: ChatEvent[], streamId: string) => void
  // apply_fix human-in-the-loop: a proposal awaits the user's decision, and
  // its resolution (applying/applied/declined/timeout/error) lands here.
  onFixProposal?: (proposal: FixProposalPayload, streamId: string) => void
  // `items` carries a BATCH decision's per-fix outcomes (parseChatEvent builds
  // it from the wire's items[]). Omitted for single-fix decisions, whose one
  // item mirrors the card status.
  onFixDecision?: (
    proposalId: string,
    status: string,
    message: string | undefined,
    streamId: string,
    items?: {ruleId: string; status: string; message?: string}[],
  ) => void
  // Delta-resume path; when absent, resumeInto falls back to a full onReplace.
  onAppend?: (delta: string, streamId: string) => void
  // UTF-8 byte length of accumulated text, so a delta-resume requests only the tail.
  getAccLength?: (streamId: string) => number
}

// A rate from a resume/begin response: that body is not validated like a
// done event (lib/chatEvent), so only a finite number counts.
function rateOf(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined
}

// doneArg builds onDone's optional meta argument from a done event's fields
// (`src` — or a resume/begin result) plus what the resume that finished the
// stream reported (`resumed`: its served model and billed rates win when
// present). Undefined when nothing was reported, so a plain done stays a
// three-argument call.
function doneArg(src: DoneMeta, resumed?: DoneMeta): DoneMeta | undefined {
  const meta: DoneMeta = {}
  for (const from of resumed ? [src, resumed] : [src]) {
    if (typeof from.servedModel === 'string' && from.servedModel) meta.servedModel = from.servedModel
    const inRate = rateOf(from.inputCostPerM)
    if (inRate !== undefined) meta.inputCostPerM = inRate
    const outRate = rateOf(from.outputCostPerM)
    if (outRate !== undefined) meta.outputCostPerM = outRate
  }
  return Object.keys(meta).length > 0 ? meta : undefined
}

// errorUsageFromEvent wraps a parsed SSE error event's usage fields: present
// only when the backend sent at least one, so an unbilled error stays a
// three-argument onError call. Zero counts are dropped — a resume/begin
// result reports 0/0 for a failure the server never counted (its
// streamCtl was never markDone'd), and 0 adds nothing to any total.
function errorUsageFromEvent(ev: ErrorUsage): ErrorUsage | undefined {
  const out: ErrorUsage = {}
  if (ev.tokensIn) out.tokensIn = ev.tokensIn
  if (ev.tokensOut) out.tokensOut = ev.tokensOut
  if (ev.shared) out.shared = true
  if (ev.meta) out.meta = ev.meta
  return Object.keys(out).length > 0 ? out : undefined
}

// errorUsageFromResult is errorUsageFromEvent for a /begin or /resume
// result: the same information travels flat (tokens, errorShared, served
// model and billed rates).
function errorUsageFromResult(res: {tokensIn?: number; tokensOut?: number; errorShared?: boolean} & DoneMeta) {
  return errorUsageFromEvent({
    tokensIn: res.tokensIn,
    tokensOut: res.tokensOut,
    shared: res.errorShared,
    meta: doneArg(res),
  })
}

// Per-stream live state: SSE unsubscriber + dropped-chunk bookkeeping. The
// done event carries the emitted chunk count; a mismatch (or a reconnect)
// triggers an authoritative resume before finishing.
interface StreamSub {
  unsub: () => void
  received: number
  countsInvalid: boolean
  stallTimer: ReturnType<typeof setTimeout> | null
  probeMisses: number
}

// Subscribes to chat stream events — one independent registration per stream
// (chat thread), plus one shared connection-state listener that resumes all
// registered streams after an SSE reconnect.
export function useStreamingMessage(handler: StreamHandler) {
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  const subsRef = useRef(new Map<string, StreamSub>())
  const unregisterConnRef = useRef<(() => void) | null>(null)
  const isCanceledRef = useRef(false)
  // OPEN_WAIT timers, cleared on unmount to avoid dangling closures.
  const openWaitTimersRef = useRef(new Set<ReturnType<typeof setTimeout>>())

  // armStall/probeStall reference each other, so the probe goes through a ref.
  const probeStallRef = useRef<(streamId: string) => void>(() => {})
  const armStall = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (!sub) return
    if (sub.stallTimer) clearTimeout(sub.stallTimer)
    sub.stallTimer = setTimeout(() => probeStallRef.current(streamId), STALL_PROBE_MS)
  }, [])

  // Fetches a stream's buffered state and replays it through the handlers.
  // 'delta' (reconnect): acc text is a clean prefix — request only the tail.
  // 'full' (mismatch): possible gaps — fetch the whole buffer and onReplace.
  const resumeInto = useCallback(
    (
      streamId: string,
      opts?: {
        finish?: {tokensOut: number; tokensIn: number; meta?: DoneMeta}
        mode?: 'delta' | 'full'
        // F-21: called when the stream turns out to be gone (403/404 —
        // retention expired) and no done-verification `finish` applies; the
        // reload-reattach path drops its slot and refetches the history.
        onGone?: () => void
      },
    ) => {
      const finish = opts?.finish
      const mode = opts?.mode ?? 'full'
      const useDelta = mode === 'delta' && handlerRef.current.onAppend && handlerRef.current.getAccLength
      const from = useDelta ? (handlerRef.current.getAccLength?.(streamId) ?? 0) : 0
      chatApi
        .resumeStream(streamId, from)
        .then(res => {
          if (!subsRef.current.has(streamId)) return
          if (res.text) {
            if (useDelta) {
              // Live chunks arriving between capturing `from` and the response
              // would overlap the tail — fall back to a full replace.
              const now = handlerRef.current.getAccLength?.(streamId) ?? 0
              if (now > from) {
                chatApi
                  .resumeStream(streamId, 0)
                  .then(fullRes => {
                    if (!subsRef.current.has(streamId)) return
                    if (fullRes.text) handlerRef.current.onReplace(fullRes.text, streamId)
                  })
                  .catch(() => {})
                return
              }
              handlerRef.current.onAppend!(res.text, streamId)
            } else {
              handlerRef.current.onReplace(res.text, streamId)
            }
          }
          // Reconnect replay: rebuild the agentic slot state (tool trail,
          // pending approval cards) from the journaled events — events
          // emitted while disconnected never arrived over SSE.
          if (res.events) {
            handlerRef.current.onResumeState?.(parseResumeEvents(res.events), streamId)
          }
          if (res.error) {
            handlerRef.current.onError(res.error, streamId, res.errorCode, errorUsageFromResult(res))
          } else if (res.done || finish) {
            handlerRef.current.onDone(
              res.tokensOut || finish?.tokensOut || 0,
              res.tokensIn || finish?.tokensIn || 0,
              streamId,
              doneArg(finish?.meta ?? {}, res),
            )
          } else {
            armStall(streamId)
          }
        })
        .catch(() => {
          // Stream gone: a done-verification pass still finishes with what
          // accumulated; reconnect probes just drop; the reload-reattach
          // path (no finish) reports the loss so the caller can refetch.
          if (!subsRef.current.has(streamId)) return
          if (finish) handlerRef.current.onDone(finish.tokensOut, finish.tokensIn, streamId, finish.meta)
          else opts?.onGone?.()
        })
    },
    [armStall],
  )

  // Clears one stream's listener after natural completion — unlike cancel, it
  // does NOT cancel on the backend.
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

  const cancel = useCallback(
    (streamId: string) => {
      chatApi.cancelStream(streamId).catch(err => {
        logger.warn('Failed to cancel stream', err)
      })
      teardownStream(streamId)
    },
    [teardownStream],
  )

  // Fires when a stream saw no event for STALL_PROBE_MS: resume returns the
  // authoritative state (dropped terminal event or dead worker), so finish or
  // error from that instead of spinning forever.
  const probeStall = useCallback(
    (streamId: string) => {
      const sub = subsRef.current.get(streamId)
      if (!sub) return
      sub.stallTimer = null
      if (getEventConnectionState() !== 'open') {
        // Disconnected: the reconnect listener owns recovery; keep watching.
        armStall(streamId)
        return
      }
      sub.countsInvalid = true
      const before = handlerRef.current.getAccLength?.(streamId) ?? 0
      const miss = () => {
        if (++sub.probeMisses >= STALL_PROBE_LIMIT) {
          handlerRef.current.onError(i18n.t('chat:errors.stalled'), streamId)
        } else {
          armStall(streamId)
        }
      }
      chatApi
        .resumeStream(streamId, 0)
        .then(res => {
          if (!subsRef.current.has(streamId)) return
          if (res.error) {
            handlerRef.current.onError(res.error, streamId, res.errorCode, errorUsageFromResult(res))
            return
          }
          if (res.text) handlerRef.current.onReplace(res.text, streamId)
          if (res.done) {
            handlerRef.current.onDone(res.tokensOut || 0, res.tokensIn || 0, streamId, doneArg(res))
            return
          }
          if (res.text && utf8ByteLength(res.text) > before) {
            // Still generating, just slowly (or the live chunks were dropped).
            sub.probeMisses = 0
            armStall(streamId)
          } else {
            miss()
          }
        })
        .catch(() => {
          // Unknown stream or failed probe: count a miss so transient failures
          // don't kill a live stream but a lost one errors out.
          if (!subsRef.current.has(streamId)) return
          miss()
        })
    },
    [armStall],
  )

  useEffect(() => {
    probeStallRef.current = probeStall
  }, [probeStall])

  // Subscribes the per-stream SSE listener (+ the shared recovery hook) and
  // waits for the connection to be 'open'. begin=true also calls /chat/begin
  // (legacy handshake: unblocks emission, fetches buffered terminal state for
  // streams that finished pre-subscription); begin=false skips it — the caller
  // supplies a clientStreamId and POSTs create after this returns.
  const registerStream = useCallback(
    async (streamId: string, begin = true) => {
      // One shared recovery listener: after a reconnect, resume every stream
      // (finished streams are retained briefly, so even completed ones recover
      // their final text instead of hanging the UI).
      if (!unregisterConnRef.current) {
        let wasReconnecting = false
        unregisterConnRef.current = subscribeConnectionState((state: EventConnectionState) => {
          // Only 'reconnecting' counts — 'connecting' is the initial dial and
          // would fire a spurious delta-resume on first connect.
          if (state === 'reconnecting') {
            wasReconnecting = true
          } else if (state === 'open' && wasReconnecting) {
            wasReconnecting = false
            for (const [sid, s] of subsRef.current) {
              s.countsInvalid = true
              resumeInto(sid, {mode: 'delta'})
            }
          }
        })
      }

      const sub: StreamSub = {unsub: () => {}, received: 0, countsInvalid: false, stallTimer: null, probeMisses: 0}

      const unsub = await subscribeToEvents((event: {name: string; data: unknown}) => {
        if (event.name !== 'chat:event') return
        // Boundary-validate before any store write; malformed frames drop.
        if (chatEventStreamId(event.data) !== streamId) return
        const parsed = parseChatEvent(event.data)
        if (!parsed) return

        switch (parsed.kind) {
          case 'chunk':
            sub.received++
            sub.probeMisses = 0
            armStall(streamId)
            handlerRef.current.onChunk(parsed.content, streamId)
            break
          case 'done': {
            if (sub.stallTimer) {
              clearTimeout(sub.stallTimer)
              sub.stallTimer = null
            }
            const meta = doneArg(parsed)
            if (sub.countsInvalid || (parsed.chunks !== undefined && parsed.chunks !== sub.received)) {
              resumeInto(streamId, {
                finish: {tokensOut: parsed.tokensOut, tokensIn: parsed.tokensIn, meta},
                mode: 'full',
              })
            } else {
              handlerRef.current.onDone(parsed.tokensOut, parsed.tokensIn, streamId, meta)
            }
            break
          }
          case 'error':
            if (sub.stallTimer) {
              clearTimeout(sub.stallTimer)
              sub.stallTimer = null
            }
            handlerRef.current.onError(parsed.message, streamId, parsed.code, errorUsageFromEvent(parsed))
            break
          case 'heartbeat':
            // The server is alive but silent (awaiting a fix decision, running
            // a slow tool or re-analysis, a model thinking before its first
            // token). Not a chunk — it must not count toward `received`.
            sub.probeMisses = 0
            armStall(streamId)
            break
          case 'tool':
            sub.probeMisses = 0
            armStall(streamId)
            handlerRef.current.onToolStatus?.(parsed.label, streamId)
            break
          case 'tool-result':
            sub.probeMisses = 0
            armStall(streamId)
            handlerRef.current.onToolResult?.(parsed, streamId)
            break
          case 'fix-proposal':
            sub.probeMisses = 0
            armStall(streamId)
            handlerRef.current.onFixProposal?.(parsed, streamId)
            break
          case 'fix-decision':
            sub.probeMisses = 0
            armStall(streamId)
            handlerRef.current.onFixDecision?.(parsed.proposalId, parsed.status, parsed.message, streamId, parsed.items)
            break
        }
      })
      sub.unsub = unsub

      // Unmounted mid-subscribe: returning false tells executeSend not to
      // create the backend stream (it would emit to a dead listener).
      if (isCanceledRef.current) {
        unsub()
        return false
      }
      subsRef.current.set(streamId, sub)
      armStall(streamId)

      // Wait for 'open' before beginning: emitting while still dialing drops
      // the initial chunks. Bounded so an unreachable backend surfaces an error
      // instead of pinning the thinking indicator forever.
      if (getEventConnectionState() !== 'open') {
        await new Promise<void>((resolve, reject) => {
          let unsubConn: (() => void) | null = null
          let settled = false
          const settle = (err?: Error) => {
            if (settled) return
            settled = true
            clearTimeout(timer)
            openWaitTimersRef.current.delete(timer)
            unsubConn?.()
            if (err) reject(err)
            else resolve()
          }
          const timer = setTimeout(() => {
            if (isCanceledRef.current) {
              settle()
              return
            }
            teardownStream(streamId)
            settle(new Error(i18n.t('chat:errors.cannotConnect')))
          }, OPEN_WAIT_TIMEOUT_MS)
          openWaitTimersRef.current.add(timer)
          const check = (state: EventConnectionState) => {
            if (state === 'open' || isCanceledRef.current) settle()
          }
          unsubConn = subscribeConnectionState(check)
          // check runs synchronously on subscribe; settle may predate the
          // unsubscriber being in scope.
          if (settled) unsubConn()
        })
      }

      if (isCanceledRef.current) return false

      if (!begin) return true

      // A beginStream failure leaves the backend goroutine blocked until its
      // stream-cap timeout with no event — propagate so the caller cleans up.
      const res = await chatApi.beginStream(streamId)

      // Fail-fast pre-stream errors emit their terminal event before our
      // subscription exists; /begin returns that buffered state — deliver it.
      if (res?.status === 'finished') {
        if (isCanceledRef.current || !subsRef.current.has(streamId)) return false
        if (res.text) {
          handlerRef.current.onReplace(res.text, streamId)
        }
        if (res.error) {
          handlerRef.current.onError(res.error, streamId, res.errorCode, errorUsageFromResult(res))
        } else if (res.done) {
          handlerRef.current.onDone(res.tokensOut || 0, res.tokensIn || 0, streamId, doneArg(res))
        }
      }
      return true
    },
    [resumeInto, armStall, teardownStream],
  )

  useEffect(() => {
    isCanceledRef.current = false
    const subs = subsRef.current
    const openWaitTimers = openWaitTimersRef.current
    return () => {
      isCanceledRef.current = true
      for (const [sid, s] of subs) {
        if (s.stallTimer) clearTimeout(s.stallTimer)
        s.unsub()
        chatApi.cancelStream(sid).catch(() => {})
      }
      subs.clear()
      for (const t of openWaitTimers) clearTimeout(t)
      openWaitTimers.clear()
      if (unregisterConnRef.current) {
        unregisterConnRef.current()
        unregisterConnRef.current = null
      }
    }
  }, [])

  // resumeInto is exposed for the engine's reload-reattach path (F-21);
  // everything else consumes it through reconnects and probes.
  return {registerStream, cancel, teardownStream, resumeInto}
}
