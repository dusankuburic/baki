import {useCallback} from 'react'
import type {MutableRefObject} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {utf8ByteLength} from '@/lib/utf8'
import type {ToolResultPayload} from '@/lib/chatEvent'
import type {ChatStoreState, StreamAcc, AccMeta} from './types'

// The store actions and the SSE teardown hook the accumulator needs. They
// are stable references (store actions and a ref), so the callbacks below
// keep their original identities.
interface AccDeps {
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  teardownRef: MutableRefObject<(streamId: string) => void>
  updateStream: ChatStoreState['updateStream']
  setStreamMeta: ChatStoreState['setStreamMeta']
  addToolCall: ChatStoreState['addToolCall']
}

// useStreamAccumulators owns the per-stream accumulation map: the growing
// text plus the RAF batching, and the store updates that mirror it onto the
// thread's stream slot. The map entry doubles as the stale-event guard — a
// late event whose streamId has no entry is ignored.
export function useStreamAccumulators({streamAccRef, teardownRef, updateStream, setStreamMeta, addToolCall}: AccDeps) {
  const flushAcc = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.raf = null
      // One atomic store update (text + tokens) instead of two set() calls —
      // halves per-frame subscriber notifications during streaming.
      updateStream(acc.threadId, {text: acc.text, tokens: Math.ceil(acc.text.length / 4)})
    },
    [streamAccRef, updateStream],
  )

  const onChunk = useCallback(
    (text: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.text += text
      // Answer text resuming after a tool phase clears the stale
      // "Using tool…" pulse — without this the last tool's label stayed lit
      // under the entire final answer.
      const slot = useChatStore.getState().streams[acc.threadId]
      const patch: Partial<import('@/stores/chatStore').StreamSlot> = {tokens: Math.ceil(acc.text.length / 4)}
      if (slot?.toolStatus != null) patch.toolStatus = null
      // Answer text on screen means no thinking indicator either — a
      // re-armed one (onToolResult) can never stick past the next chunk.
      if (slot?.isThinking) patch.isThinking = false
      if (!acc.first) {
        // First chunk: flush synchronously so the slot text appears in the same
        // React render that clears the thinking indicator — no empty-frame gap.
        acc.first = true
        patch.text = acc.text
        patch.isThinking = false
        updateStream(acc.threadId, patch)
      } else {
        if (patch.toolStatus !== undefined || patch.isThinking !== undefined) {
          updateStream(acc.threadId, {toolStatus: null, isThinking: false})
        }
        if (acc.raf === null) {
          acc.raf = requestAnimationFrame(() => flushAcc(streamId))
        }
      }
    },
    [streamAccRef, flushAcc, updateStream],
  )

  const onReplace = useCallback(
    (text: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.text = text
      acc.first = true
      updateStream(acc.threadId, {text, isThinking: false, tokens: Math.ceil(text.length / 4)})
    },
    [streamAccRef, updateStream],
  )

  const onToolStatus = useCallback(
    (label: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      setStreamMeta(acc.threadId, {isThinking: false, toolStatus: label})
    },
    [streamAccRef, setStreamMeta],
  )

  // onToolResult records one finished tool execution on the thread's stream
  // slot — the live view of the trail that commitAssistantMessage pins onto
  // the permanent assistant message.
  const onToolResult = useCallback(
    (record: ToolResultPayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      addToolCall(acc.threadId, {
        name: record.name,
        label: record.label,
        ok: record.ok,
        durationMs: record.durationMs,
        summary: record.summary,
      })
      // The finished call's "running" label must not keep pulsing through
      // the (possibly minutes-long) silent reasoning phase that follows.
      // Clear it only while it is still THIS call's — a second tool may
      // already be running under the same label — and re-arm the thinking
      // indicator only before any answer text exists (a second indicator
      // above an already-visible bubble would be wrong).
      const slot = useChatStore.getState().streams[acc.threadId]
      if (slot && slot.toolStatus === record.label) {
        setStreamMeta(acc.threadId, {toolStatus: null, isThinking: !acc.first})
      }
    },
    [streamAccRef, addToolCall, setStreamMeta],
  )

  // takeAcc removes and returns a finished stream's accumulation state,
  // cancelling any pending flush. The store slot must still match the
  // streamId — a thread closed mid-stream drops its slot, and its result is
  // then discarded rather than committed to a dead thread.
  const takeAcc = useCallback(
    (streamId: string): StreamAcc | null => {
      // Torn down even with no accumulator left: a subscription that registered
      // after the acc was dropped (registerStream was mid-flight) must not linger.
      teardownRef.current(streamId)
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return null
      streamAccRef.current.delete(streamId)
      if (acc.raf !== null) {
        cancelAnimationFrame(acc.raf)
        acc.raf = null
      }
      const slot = useChatStore.getState().streams[acc.threadId]
      if (!slot || slot.streamId !== streamId) return null
      return acc
    },
    [streamAccRef, teardownRef],
  )

  // dropAcc clears an acc entry on a send that never completed (stale-gen
  // return, create-POST failure). It cancels any pending RAF so a straggler
  // flush can't fire after the slot is gone. Unlike takeAcc it does NOT touch
  // the store slot or tear down SSE (the caller does cancelStream/endStream).
  const dropAcc = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      if (acc.raf !== null) cancelAnimationFrame(acc.raf)
      streamAccRef.current.delete(streamId)
    },
    [streamAccRef],
  )

  // beginAcc primes the accumulator for a stream that's about to start, so
  // the first chunk event finds its key.
  const beginAcc = useCallback(
    (streamId: string, threadId: string, meta: AccMeta) => {
      const linked = useChatStore.getState().streams[threadId]?.streamId === streamId
      streamAccRef.current.set(streamId, {threadId, text: '', raf: null, first: false, linked, ...meta})
    },
    [streamAccRef],
  )

  // onAppend adds a delta-resume tail to the stream's accumulated text and
  // flushes it synchronously to the slot (no RAF — a resume delivers a known
  // tail after a reconnect, not a stream of live chunks). Used by
  // useStreamingMessage.resumeInto in 'delta' mode.
  const onAppend = useCallback(
    (delta: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc || !delta) return
      acc.text += delta
      acc.first = true
      if (acc.raf !== null) {
        cancelAnimationFrame(acc.raf)
        acc.raf = null
      }
      updateStream(acc.threadId, {text: acc.text, isThinking: false, tokens: Math.ceil(acc.text.length / 4)})
    },
    [streamAccRef, updateStream],
  )

  // getAccLength reports the accumulated text's UTF-8 BYTE length for delta
  // resume — the backend slices by byte offset, not UTF-16 .length.
  const getAccLength = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      return acc ? utf8ByteLength(acc.text) : 0
    },
    [streamAccRef],
  )

  return {streamAccRef, onChunk, onReplace, onToolStatus, onToolResult, takeAcc, dropAcc, beginAcc, onAppend, getAccLength}
}
