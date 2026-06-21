import {memo, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import MessageBubble from './MessageBubble'

// Reads streamingText directly from the store so AITab does not re-render
// on every chunk — only StreamingBubble does.
const StreamingBubble = memo(() => {
  const streamingText = useChatStore(s => s.streamingText)
  const streamingMessageId = useChatStore(s => s.streamingMessageId)
  // Stable timestamp per stream (avoids the displayed time flickering at 60fps)
  const timestamp = useMemo(() => new Date().toISOString(), [streamingMessageId])

  if (!streamingText) return null
  return (
    <MessageBubble
      message={{
        id: streamingMessageId || 'streaming',
        role: 'assistant',
        content: streamingText,
        timestamp,
      }}
      isStreaming
    />
  )
})
StreamingBubble.displayName = 'StreamingBubble'
export default StreamingBubble
