import {useCallback} from 'react'
import type {MutableRefObject} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {utf8ByteLength} from '@/lib/utf8'
import type {NoticePayload, ToolResultPayload} from '@/lib/chatEvent'
import {NOTICE_HISTORY_TRUNCATED} from '@/lib/chatEvent'
import type {ChatStoreState, StreamAcc, AccMeta} from './types'

interface AccDeps {
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  teardownRef: MutableRefObject<(streamId: string) => void>
  updateStream: ChatStoreState['updateStream']
  setStreamMeta: ChatStoreState['setStreamMeta']
  addToolCall: ChatStoreState['addToolCall']
}

export function useStreamAccumulators({streamAccRef, teardownRef, updateStream, setStreamMeta, addToolCall}: AccDeps) {
  const flushAcc = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.raf = null
      updateStream(acc.threadId, {text: acc.text, tokens: Math.ceil(acc.text.length / 4)})
    },
    [streamAccRef, updateStream],
  )

  const onChunk = useCallback(
    (text: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.text += text
      const slot = useChatStore.getState().streams[acc.threadId]
      const patch: Partial<import('@/stores/chatStore').StreamSlot> = {tokens: Math.ceil(acc.text.length / 4)}
      if (slot?.toolStatus != null) patch.toolStatus = null
      if (slot?.isThinking) patch.isThinking = false
      if (!acc.first) {
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
      const slot = useChatStore.getState().streams[acc.threadId]
      if (slot && slot.toolStatus === record.label) {
        setStreamMeta(acc.threadId, {toolStatus: null, isThinking: !acc.first})
      }
    },
    [streamAccRef, addToolCall, setStreamMeta],
  )

  const onNotice = useCallback(
    (notice: NoticePayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      if (notice.notice === NOTICE_HISTORY_TRUNCATED && notice.keptFrom != null && notice.keptFrom > 0) {
        setStreamMeta(acc.threadId, {truncated: notice.keptFrom})
      }
    },
    [streamAccRef, setStreamMeta],
  )

  const takeAcc = useCallback(
    (streamId: string): StreamAcc | null => {
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

  const dropAcc = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      if (acc.raf !== null) cancelAnimationFrame(acc.raf)
      streamAccRef.current.delete(streamId)
    },
    [streamAccRef],
  )

  const beginAcc = useCallback(
    (streamId: string, threadId: string, meta: AccMeta) => {
      const linked = useChatStore.getState().streams[threadId]?.streamId === streamId
      streamAccRef.current.set(streamId, {threadId, text: '', raf: null, first: false, linked, ...meta})
    },
    [streamAccRef],
  )

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

  const getAccLength = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      return acc ? utf8ByteLength(acc.text) : 0
    },
    [streamAccRef],
  )

  return {
    streamAccRef,
    onChunk,
    onReplace,
    onToolStatus,
    onToolResult,
    onNotice,
    takeAcc,
    dropAcc,
    beginAcc,
    onAppend,
    getAccLength,
  }
}
