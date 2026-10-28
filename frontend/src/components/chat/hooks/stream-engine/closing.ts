import {useCallback, useEffect, useRef} from 'react'
import type {MutableRefObject} from 'react'

/**
 * Streams stopped while an approved fix was being applied (R-approvals-5). The message is committed at the stop with
 * the card as it was ("applying"); the server finishes the apply before it ends the stream, so the subscription is
 * kept until the stream's terminal event (or CLOSING_GRACE_MS) and the late fix_decision settles the committed card.
 */
export const CLOSING_GRACE_MS = 6 * 60_000

export interface ClosingStream {
  threadId: string
  messageId: string
}

export function useClosingStreams(teardownRef: MutableRefObject<(streamId: string) => void>) {
  const closingRef = useRef(new Map<string, ClosingStream & {timer: ReturnType<typeof setTimeout>}>())

  const settle = useCallback(
    (streamId: string) => {
      const entry = closingRef.current.get(streamId)
      if (!entry) return false
      clearTimeout(entry.timer)
      closingRef.current.delete(streamId)
      teardownRef.current(streamId)
      return true
    },
    [teardownRef],
  )

  const linger = useCallback(
    (streamId: string, threadId: string, messageId: string) => {
      const prev = closingRef.current.get(streamId)
      if (prev) clearTimeout(prev.timer)
      const timer = setTimeout(() => settle(streamId), CLOSING_GRACE_MS)
      closingRef.current.set(streamId, {threadId, messageId, timer})
    },
    [settle],
  )

  const closing = useCallback((streamId: string): ClosingStream | undefined => closingRef.current.get(streamId), [])

  // A thread moved to a new key (rekeyThread): the cards still being applied settle on it there.
  const rekey = useCallback((oldThreadId: string, newThreadId: string) => {
    for (const entry of closingRef.current.values()) {
      if (entry.threadId === oldThreadId) entry.threadId = newThreadId
    }
  }, [])

  useEffect(() => {
    const entries = closingRef.current
    return () => {
      for (const e of entries.values()) clearTimeout(e.timer)
      entries.clear()
    }
  }, [])

  return {linger, settle, closing, rekey}
}

export type ClosingStreams = ReturnType<typeof useClosingStreams>
