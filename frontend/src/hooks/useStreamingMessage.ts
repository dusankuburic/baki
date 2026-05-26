import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {subscribeToEvents} from '@/api/client'
import {useChatStore} from '@/stores/chatStore'

interface StreamHandler {
  onChunk: (text: string) => void
  onDone: (tokensOut: number, tokensIn: number) => void
  onError: (error: string) => void
}

export function useStreamingMessage(handler: StreamHandler) {
  const unregisterRef = useRef<(() => void) | null>(null)
  const handlerRef = useRef(handler)
  handlerRef.current = handler
  const activeStreamId = useChatStore(s => s.activeStreamId)

  const registerStream = useCallback(async (streamId: string) => {
    if (unregisterRef.current) {
      unregisterRef.current()
      unregisterRef.current = null
    }

    // Await the subscription so the SSE connection is fully established
    // before signalling the backend to start streaming. Without this await,
    // beginStream can fire while the EventSource is still connecting and all
    // events get dropped, leaving the UI frozen in the streaming state.
    const unsub = await subscribeToEvents((event: any) => {
      if (event.name !== 'chat:event') return
      const payload = event.data || {}
      if (payload.streamId !== streamId) return

      const type = payload.type
      const data = payload.data || {}

      switch (type) {
        case 'chunk':
          handlerRef.current.onChunk(data.text || '')
          break
        case 'done':
          handlerRef.current.onDone(data.totalTokens || 0, data.tokensIn || 0)
          break
        case 'error':
          handlerRef.current.onError(data.error || 'Unknown error')
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
  }, [activeStreamId])

  useEffect(() => {
    return () => {
      if (unregisterRef.current) {
        unregisterRef.current()
        unregisterRef.current = null
      }
    }
  }, [])

  return {registerStream, cancel}
}
