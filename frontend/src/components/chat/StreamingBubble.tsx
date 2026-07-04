import {memo, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import MessageBubble from './MessageBubble'

// Reads the active thread's streaming slot directly from the store so AITab
// does not re-render on every chunk — only StreamingBubble does. The slot is
// per-thread (chatStore.streams), so this naturally tracks whichever thread
// is active; background threads stream without rendering here.
const StreamingBubble = memo(() => {
  const slot = useChatStore(s => (s.activeThreadId ? s.streams[s.activeThreadId] : undefined))
  // Stable timestamp per stream (avoids the displayed time flickering at 60fps)
  const timestamp = useMemo(() => new Date().toISOString(), [slot?.messageId])

  if (!slot || !slot.text) return null
  return (
    <MessageBubble
      message={{
        id: slot.messageId || 'streaming',
        role: 'assistant',
        content: slot.text,
        timestamp,
      }}
      isStreaming
    />
  )
})
StreamingBubble.displayName = 'StreamingBubble'
export default StreamingBubble
