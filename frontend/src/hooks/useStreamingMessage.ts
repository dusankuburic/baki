import {useEffect, useRef, useCallback} from 'react'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {subscribeToEvents, subscribeConnectionState, EventConnectionState, getEventConnectionState} from '@/api/client'
import {useChatStore} from '@/stores/chatStore'

interface StreamHandler {
  onChunk: (text: string) => void
  onReplace: (text: string) => void
  onDone: (tokensOut: number, tokensIn: number) => void
  onError: (error: string) => void
  // onToolStatus is emitted during the read-only tool/agent loop with a short
  // label for the current step (e.g. "Searching flow"). Optional.
  onToolStatus?: (label: string) => void
}

export function useStreamingMessage(handler: StreamHandler) {
  const unregisterRef = useRef<(() => void) | null>(null)
  const unregisterConnRef = useRef<(() => void) | null>(null)
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  const activeStreamId = useChatStore(s => s.activeStreamId)
  const streamIdRef = useRef<string | null>(null)
  const isCanceledRef = useRef(false)

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
    isCanceledRef.current = false

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

    const unsub = await subscribeToEvents((event: { name: string; data: unknown }) => {
      if (event.name !== 'chat:event') return
      const payload = event.data as Record<string, unknown> | null
      if (!payload || payload.streamId !== streamId) return

      const type = payload.type as string
      const data = (payload.data as Record<string, unknown>) || {}

      switch (type) {
        case 'chunk':
          handlerRef.current.onChunk((data.content as string) || '')
          break
        case 'done':
          handlerRef.current.onDone((data.tokensOut as number) || 0, (data.tokensIn as number) || 0)
          break
        case 'error':
          handlerRef.current.onError((data.message as string) || 'Unknown error')
          break
        case 'tool':
          handlerRef.current.onToolStatus?.((data.label as string) || (data.name as string) || 'Using tool')
          break
      }
    })

    unregisterRef.current = unsub

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

    // If beginStream fails the backend goroutine blocks on awaitStart() until its
    // 5-minute context timeout fires — with no event emitted afterward. Propagate
    // the error so the caller (executeSend) can clean up the streaming state.
    await chatApi.beginStream(streamId)
  }, [])

  const cancel = useCallback(() => {
    isCanceledRef.current = true
    if (activeStreamId) {
      chatApi.cancelStream(activeStreamId).catch((err) => { logger.warn('Failed to cancel stream', err) })
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
