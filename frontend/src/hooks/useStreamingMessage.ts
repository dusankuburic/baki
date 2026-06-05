import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState} from '@/api/client'
import {useChatStore} from '@/stores/chatStore'

interface StreamHandler {
  onChunk: (text: string) => void
  onReplace: (text: string) => void
  onDone: (tokensOut: number, tokensIn: number) => void
  onError: (error: string) => void
}

export function useStreamingMessage(handler: StreamHandler) {
  const unregisterRef = useRef<(() => void) | null>(null)
  const unregisterConnRef = useRef<(() => void) | null>(null)
  const handlerRef = useRef(handler)
  handlerRef.current = handler
  const activeStreamId = useChatStore(s => s.activeStreamId)
  const streamIdRef = useRef<string | null>(null)

  const registerStream = useCallback(async (streamId: string) => {
    if (unregisterRef.current) {
      unregisterRef.current()
      unregisterRef.current = null
    }
    if (unregisterConnRef.current) {
      unregisterConnRef.current()
      unregisterConnRef.current = null
    }
    streamIdRef.current = streamId

    let wasReconnecting = false

    unregisterConnRef.current = subscribeConnectionState((state: EventConnectionState) => {
      if (state === 'reconnecting' || state === 'connecting') {
        wasReconnecting = true
      } else if (state === 'open' && wasReconnecting) {
        wasReconnecting = false
        // Connection recovered, fetch full buffered state. The backend retains
        // a finished stream briefly, so even if it completed while we were
        // disconnected we recover the final text and a done/error signal
        // (otherwise the UI would hang in the streaming state).
        if (streamIdRef.current) {
          chatApi.resumeStream(streamIdRef.current).then(res => {
            if (res.text) {
              handlerRef.current.onReplace(res.text)
            }
            if (res.error) {
              handlerRef.current.onError(res.error)
            } else if (res.done) {
              handlerRef.current.onDone(res.tokensOut, res.tokensIn)
            }
          }).catch(() => {
            // Silently fail if stream is no longer available
          })
        }
      }
    })

    const unsub = await subscribeToEvents((event: any) => {
      if (event.name !== 'chat:event') return
      const payload = event.data || {}
      if (payload.streamId !== streamId) return

      const type = payload.type
      const data = payload.data || {}

      switch (type) {
        case 'chunk':
          handlerRef.current.onChunk(data.content || '')
          break
        case 'done':
          handlerRef.current.onDone(data.tokensOut || 0, data.tokensIn || 0)
          break
        case 'error':
          handlerRef.current.onError(data.message || 'Unknown error')
          break
      }
    })

    unregisterRef.current = unsub
    chatApi.beginStream(streamId).catch(() => {})
  }, [])

  const cancel = useCallback(() => {
    if (activeStreamId) {
      chatApi.cancelStream(activeStreamId).catch(() => {})
    }
    if (unregisterRef.current) {
      unregisterRef.current()
      unregisterRef.current = null
    }
    if (unregisterConnRef.current) {
      unregisterConnRef.current()
      unregisterConnRef.current = null
    }
    streamIdRef.current = null
  }, [activeStreamId])

  useEffect(() => {
    return () => {
      if (unregisterRef.current) unregisterRef.current()
      if (unregisterConnRef.current) unregisterConnRef.current()
    }
  }, [])

  return {registerStream, cancel}
}
