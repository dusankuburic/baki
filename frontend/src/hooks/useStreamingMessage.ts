import i18n from '@/i18n'
import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {utf8ByteLength} from '@/lib/utf8'
import {CHAT_ERROR_CODES, isStreamGone} from '@/lib/chatErrors'
import {
  parseChatEvent,
  chatEventStreamId,
  parseResumeEvents,
  FixDecisionEventItem,
  FixProposalPayload,
  ToolResultPayload,
  ChatEvent,
  DoneMeta,
  ErrorUsage,
} from '@/lib/chatEvent'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState, getEventConnectionState} from '@/api/client'

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
}

function rateOf(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined
}

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

interface StreamSub {
  unsub: () => void
  received: number
  countsInvalid: boolean
  stallTimer: ReturnType<typeof setTimeout> | null
  probeMisses: number
  live: boolean
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
    if (!sub) return
    if (sub.stallTimer) clearTimeout(sub.stallTimer)
    sub.stallTimer = setTimeout(() => probeStallRef.current(streamId), STALL_PROBE_MS)
  }, [])

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
      chatApi
        .resumeStream(streamId, from)
        .then(res => {
          const sub = subsRef.current.get(streamId)
          if (!sub) return
          sub.live = true
          if (res.text) {
            if (useDelta) {
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
        .catch((err: unknown) => {
          if (!subsRef.current.has(streamId)) return
          if (finish) handlerRef.current.onDone(finish.tokensOut, finish.tokensIn, streamId, finish.meta)
          else opts?.onGone?.(err)
        })
    },
    [armStall],
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

  const cancel = useCallback(
    (streamId: string) => {
      chatApi.cancelStream(streamId).catch(err => {
        logger.warn('Failed to cancel stream', err)
      })
      teardownStream(streamId)
    },
    [teardownStream],
  )

  const probeStall = useCallback(
    (streamId: string) => {
      const sub = subsRef.current.get(streamId)
      if (!sub) return
      sub.stallTimer = null
      if (getEventConnectionState() !== 'open') {
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
            sub.probeMisses = 0
            armStall(streamId)
          } else {
            miss()
          }
        })
        .catch(() => {
          if (!subsRef.current.has(streamId)) return
          miss()
        })
    },
    [armStall],
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
    async (streamId: string, begin = true) => {
      if (!unregisterConnRef.current) {
        let wasReconnecting = false
        unregisterConnRef.current = subscribeConnectionState((state: EventConnectionState) => {
          if (state === 'reconnecting') {
            wasReconnecting = true
          } else if (state === 'open' && wasReconnecting) {
            wasReconnecting = false
            for (const [sid, s] of subsRef.current) {
              s.countsInvalid = true
              resumeInto(sid, {mode: 'delta', onGone: err => reportLost(sid, err)})
            }
          }
        })
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

      if (isCanceledRef.current) return false

      if (!begin) return true

      const res = await chatApi.beginStream(streamId)
      sub.live = true

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

  return {registerStream, confirmStream, cancel, teardownStream, resumeInto}
}
