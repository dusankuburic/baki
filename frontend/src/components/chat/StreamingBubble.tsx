import {memo, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import MessageBubble from './MessageBubble'

const StreamingBubble = memo(() => {
  const slot = useChatStore(s => (s.activeThreadId ? s.streams[s.activeThreadId] : undefined))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const timestamp = useMemo(() => new Date().toISOString(), [slot?.messageId])

  if (!slot || !slot.text) return null
  return (
    <div aria-hidden="true">
      <MessageBubble
        message={{
          id: slot.messageId || 'streaming',
          role: 'assistant',
          content: slot.text,
          timestamp,
        }}
        isStreaming
      />
    </div>
  )
})
StreamingBubble.displayName = 'StreamingBubble'
export default StreamingBubble
