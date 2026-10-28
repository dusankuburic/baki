import i18n from '@/i18n'
import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {onBackendRestarted} from '@/lib/backendRestart'
import {utf8ByteLength} from '@/lib/utf8'
import {CHAT_ERROR_CODES, isStreamGone} from '@/lib/chatErrors'
import {isStreamControlRetryable} from '@/lib/streamControl'
import {
  parseChatEvent,
  chatEventStreamId,
  parseResumeEvents,
  FixDecisionEventItem,
  FixProposalPayload,
  ToolResultPayload,
  NoticePayload,
  ChatEvent,
  DoneMeta,
  ErrorUsage,
} from '@/lib/chatEvent'
import {
  subscribeToEvents,
  subscribeConnectionState,
  subscribeEventGap,
  EventConnectionState,
  getEventConnectionState,
} from '@/api/client'

const OPEN_WAIT_TIMEOUT_MS = 15_000

const STALL_PROBE_MS = 30_000
const STALL_PROBE_LIMIT = 3

export interface StreamHandler {
  onChunk: (text: string, streamId: string) => void
  onReplace: (text: string, streamId: string) => void
  onDone: (tokensOut: number, tokensIn: number, streamId: string, meta?: DoneMeta) => void
  onError: (error: string, streamId: string, code?: string, usage?: ErrorUsage) => void
  onToolStatus?: (label: string, streamId: string) => void
  onToolResult?: (record: ToolResultPayload, streamId: string) => void
  onNotice?: (notice: NoticePayload, streamId: string) => void
  onResumeState?: (events: ChatEvent[], streamId: string) => void
  onFixProposal?: (proposal: FixProposalPayload, streamId: string) => void
  onFixDecision?: (
    proposalId: string,
    status: string,
    message: string | undefined,
    streamId: string,
    items?: FixDecisionEventItem[],
  ) => void
  onAppend?: (delta: string, streamId: string) => void
  getAccLength?: (streamId: string) => number
  /** A stop could not reach the server running the stream, after the api layer's retries; the stream is torn down here but still runs there. */
  onStopUnreachable?: (streamId: string, err?: unknown) => void
}

function doneArg(src: DoneMeta, resumed?: DoneMeta): DoneMeta | undefined {
  const meta: DoneMeta = {}
  for (const from of resumed ? [src, resumed] : [src]) {
    if (typeof from.servedModel === 'string' && from.servedModel) meta.servedModel = from.servedModel
    if (from.conversationVersion !== undefined && from.conversationBase !== undefined) {
      meta.conversationVersion = from.conversationVersion
      meta.conversationBase = from.conversationBase
    }
    // The server could not store its answer (R-tool-loop-20): the commit saves the turn from the client.
    if (from.answerStored === false) meta.answerStored = false
  }
  return Object.keys(meta).length > 0 ? meta : undefined
}

function errorUsageFromEvent(ev: ErrorUsage): ErrorUsage | undefined {
  const out: ErrorUsage = {}
  if (ev.tokensIn) out.tokensIn = ev.tokensIn
  if (ev.tokensOut) out.tokensOut = ev.tokensOut
  if (ev.shared) out.shared = true
  if (ev.meta) out.meta = ev.meta
  return Object.keys(out).length > 0 ? out : undefined
}

function errorUsageFromResult(res: {tokensIn?: number; tokensOut?: number; errorShared?: boolean} & DoneMeta) {
  return errorUsageFromEvent({
    tokensIn: res.tokensIn,
    tokensOut: res.tokensOut,
    shared: res.errorShared,
    meta: doneArg(res),
  })
}

type ResumeAnswer = Awaited<ReturnType<typeof chatApi.resumeStream>>

interface StreamSub {
  unsub: () => void
  received: number
  countsInvalid: boolean
  stallTimer: ReturnType<typeof setTimeout> | null
  probeMisses: number
  live: boolean
  // A stop was sent: the subscription stays, unprobed, until the server confirms the stop or the stream ends.
  stopping?: boolean
}

export function useStreamingMessage(handler: StreamHandler) {
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  const subsRef = useRef(new Map<string, StreamSub>())
  const unregisterConnRef = useRef<(() => void) | null>(null)
  const isCanceledRef = useRef(false)
  const openWaitTimersRef = useRef(new Set<ReturnType<typeof setTimeout>>())

  const probeStallRef = useRef<(streamId: string) => void>(() => {})
  const armStall = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (!sub || sub.stopping) return
    if (sub.stallTimer) clearTimeout(sub.stallTimer)
    sub.stallTimer = setTimeout(() => probeStallRef.current(streamId), STALL_PROBE_MS)
  }, [])

  // One resume answer, applied the same way whichever call fetched it (R-streaming-5): its text (replacing the
  // buffer, or appended after it), then its events, so the trail and the cards are current, then its end — error or
  // done (a known finish ends it too) — or, while the stream runs on, onLive (by default the next stall timer).
  // Returns whether the answer ended the stream.
  const applyResume = useCallback(
    (
      streamId: string,
      res: ResumeAnswer,
      opts: {
        mode: 'replace' | 'append'
        finish?: {tokensOut: number; tokensIn: number; meta?: DoneMeta}
        onLive?: () => void
      },
    ): boolean => {
      if (res.text) {
        if (opts.mode === 'append') handlerRef.current.onAppend?.(res.text, streamId)
        else handlerRef.current.onReplace(res.text, streamId)
      }
      if (res.events) handlerRef.current.onResumeState?.(parseResumeEvents(res.events), streamId)
      if (res.error) {
        handlerRef.current.onError(res.error, streamId, res.errorCode, errorUsageFromResult(res))
        return true
      }
      const finish = opts.finish
      if (res.done || finish) {
        handlerRef.current.onDone(
          res.tokensOut || finish?.tokensOut || 0,
          res.tokensIn || finish?.tokensIn || 0,
          streamId,
          doneArg(finish?.meta ?? {}, res),
        )
        return true
      }
      if (opts.onLive) opts.onLive()
      else armStall(streamId)
      return false
    },
    [armStall],
  )

  const resumeInto = useCallback(
    (
      streamId: string,
      opts?: {
        finish?: {tokensOut: number; tokensIn: number; meta?: DoneMeta}
        mode?: 'delta' | 'full'
        onGone?: (err: unknown) => void
      },
    ) => {
      const finish = opts?.finish
      const mode = opts?.mode ?? 'full'
      const useDelta = mode === 'delta' && handlerRef.current.onAppend && handlerRef.current.getAccLength
      const from = useDelta ? (handlerRef.current.getAccLength?.(streamId) ?? 0) : 0
      const failed = (err: unknown) => {
        if (!subsRef.current.has(streamId)) return
        if (finish) handlerRef.current.onDone(finish.tokensOut, finish.tokensIn, streamId, finish.meta)
        else opts?.onGone?.(err)
      }
      chatApi
        .resumeStream(streamId, from)
        .then(res => {
          const sub = subsRef.current.get(streamId)
          if (!sub) return
          sub.live = true
          // A live chunk landed while the delta was on its way: the offset is stale, so the whole answer is fetched
          // again and applied in full — its text, and its events and end, which the delta also carried.
          if (useDelta && res.text && (handlerRef.current.getAccLength?.(streamId) ?? 0) > from) {
            chatApi
              .resumeStream(streamId, 0)
              .then(fullRes => {
                if (!subsRef.current.has(streamId)) return
                applyResume(streamId, fullRes, {mode: 'replace', finish})
              })
              .catch(failed)
            return
          }
          applyResume(streamId, res, {mode: useDelta ? 'append' : 'replace', finish})
        })
        .catch(failed)
    },
    [applyResume],
  )

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

  // Stops a stream (R-streaming-c20). Its subscription stays until the server confirms the stop, so a stop that did
  // not get through (a relay failure, a rate limit, after the api layer's retries) still hears the stream's end,
  // which tears it down; the handler offers the stop again meanwhile. The stopping stream is not probed: what it
  // still sends has nothing left to commit.
  const cancel = useCallback(
    (streamId: string) => {
      const sub = subsRef.current.get(streamId)
      if (sub) {
        sub.stopping = true
        if (sub.stallTimer) clearTimeout(sub.stallTimer)
        sub.stallTimer = null
      }
      chatApi
        .cancelStream(streamId)
        .then(() => teardownStream(streamId))
        .catch(err => {
          logger.warn('Failed to cancel stream', err)
          // Unmounted meanwhile: its cleanup took the subscriptions, and no one is left to tell.
          if (isCanceledRef.current) return
          if (isStreamControlRetryable(err)) handlerRef.current.onStopUnreachable?.(streamId, err)
          else teardownStream(streamId)
        })
    },
    [teardownStream],
  )

  const probeStall = useCallback(
    (streamId: string) => {
      const sub = subsRef.current.get(streamId)
      if (!sub) return
      sub.stallTimer = null
      // The probe is plain HTTP: it runs while the event stream is down too, so a stream that finished meanwhile
      // commits instead of waiting for a reconnect that may be minutes away (R-streaming-6).
      sub.countsInvalid = true
      const before = handlerRef.current.getAccLength?.(streamId) ?? 0
      // Only an answered probe that shows no progress is a miss, and only while the events stream is open: then
      // events should have been arriving. While it is down, the server answering that the stream still runs is
      // all a probe can learn, so it is progress.
      const sseDown = getEventConnectionState() !== 'open'
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
          applyResume(streamId, res, {
            mode: 'replace',
            onLive: () => {
              if (sseDown || (res.text && utf8ByteLength(res.text) > before)) {
                sub.probeMisses = 0
                armStall(streamId)
              } else {
                miss()
              }
            },
          })
        })
        .catch((err: unknown) => {
          if (!subsRef.current.has(streamId)) return
          // An error status is the server's answer: no progress. No answer at all (offline, the server unreachable,
          // a timeout) says nothing about the stream: probe again later.
          const status = typeof err === 'object' && err !== null ? (err as {status?: unknown}).status : undefined
          if (typeof status === 'number' && status > 0) miss()
          else armStall(streamId)
        })
    },
    [armStall, applyResume],
  )

  useEffect(() => {
    probeStallRef.current = probeStall
  }, [probeStall])

  const reportLost = useCallback((streamId: string, err: unknown) => {
    const sub = subsRef.current.get(streamId)
    if (!sub?.live || !isStreamGone(err)) return
    handlerRef.current.onError(i18n.t('chat:errors.interrupted'), streamId, CHAT_ERROR_CODES.interrupted)
  }, [])

  const confirmStream = useCallback((streamId: string) => {
    const sub = subsRef.current.get(streamId)
    if (sub) sub.live = true
  }, [])

  const registerStream = useCallback(
    async (streamId: string) => {
      if (!unregisterConnRef.current) {
        // What a reconnect does for the streams it may have cut off: the chunks that arrived during the gap are lost, so each
        // stream resumes by offset. A replay gap (the server could not replay what was missed) is the same situation.
        const resumeAll = () => {
          for (const [sid, s] of subsRef.current) {
            s.countsInvalid = true
            resumeInto(sid, {mode: 'delta', onGone: err => reportLost(sid, err)})
          }
        }
        let wasReconnecting = false
        const offState = subscribeConnectionState((state: EventConnectionState) => {
          if (state === 'reconnecting') {
            wasReconnecting = true
          } else if (state === 'open' && wasReconnecting) {
            wasReconnecting = false
            resumeAll()
          }
        })
        const offGap = subscribeEventGap(resumeAll)
        // The backend process was replaced (R-frontend-c7): the streams it ran died with it, so each ends here as
        // interrupted, and none is resumed or cancelled against the new process, which never knew it. A stream the
        // old process never confirmed (a send still waiting to be created) is left to go on against the new one.
        const offRestart = onBackendRestarted(() => {
          for (const [sid, s] of [...subsRef.current]) {
            if (!s.live) continue
            if (s.stallTimer) clearTimeout(s.stallTimer)
            s.unsub()
            subsRef.current.delete(sid)
            handlerRef.current.onError(i18n.t('chat:errors.backendRestarted'), sid, CHAT_ERROR_CODES.backendRestarted)
          }
        })
        unregisterConnRef.current = () => {
          offState()
          offGap()
          offRestart()
        }
      }

      const sub: StreamSub = {
        unsub: () => {},
        received: 0,
        countsInvalid: false,
        stallTimer: null,
        probeMisses: 0,
        live: false,
      }

      const unsub = await subscribeToEvents((event: {name: string; data: unknown}) => {
        if (event.name !== 'chat:event') return
        if (chatEventStreamId(event.data) !== streamId) return
        const parsed = parseChatEvent(event.data)
        if (!parsed) return
        sub.live = true
        // A stopped stream's end: handed on as it is (nothing is left to resume into) and the subscription goes.
        if (sub.stopping && (parsed.kind === 'done' || parsed.kind === 'error')) {
          if (parsed.kind === 'done') {
            handlerRef.current.onDone(parsed.tokensOut, parsed.tokensIn, streamId, doneArg(parsed))
          } else {
            handlerRef.current.onError(parsed.message, streamId, parsed.code, errorUsageFromEvent(parsed))
          }
          teardownStream(streamId)
          return
        }
        if (parsed.kind !== 'done' && parsed.kind !== 'error') {
          sub.probeMisses = 0
          armStall(streamId)
        }

        switch (parsed.kind) {
          case 'chunk':
            sub.received++
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
            break
          case 'tool':
            handlerRef.current.onToolStatus?.(parsed.label, streamId)
            break
          case 'tool-result':
            handlerRef.current.onToolResult?.(parsed, streamId)
            break
          case 'notice':
            handlerRef.current.onNotice?.(parsed, streamId)
            break
          case 'fix-proposal':
            handlerRef.current.onFixProposal?.(parsed, streamId)
            break
          case 'fix-decision':
            handlerRef.current.onFixDecision?.(parsed.proposalId, parsed.status, parsed.message, streamId, parsed.items)
            break
        }
      })
      sub.unsub = unsub

      if (isCanceledRef.current) {
        unsub()
        return false
      }
      subsRef.current.set(streamId, sub)
      armStall(streamId)

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
          if (settled) unsubConn()
        })
      }

      // Gone while it waited: torn down, or ended by a backend restart.
      return !isCanceledRef.current && subsRef.current.has(streamId)
    },
    [resumeInto, armStall, teardownStream, reportLost],
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
        // A stopping stream's stop is already on its way, retries and all.
        if (!s.stopping) chatApi.cancelStream(sid).catch(() => {})
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

  return {registerStream, confirmStream, cancel, teardownStream, resumeInto}
}
