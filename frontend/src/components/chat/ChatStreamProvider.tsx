import {useEffect, useRef, type ReactNode} from 'react'
import {useTranslation} from 'react-i18next'
import {useChatStore, threadModelOf, type ChatThread, type QueuedMessage} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStreamEngine, type StreamMeta} from './hooks/useChatStreamEngine'
import {ChatStreamContext} from './chatStreamContext'
import {useOptionalToast} from '@/components/shared/Toast'
import type {ChatMessage} from '@/types'

function queuedPair(queued: QueuedMessage, thread: ChatThread): StreamMeta {
  if (queued.provider) {
    return {
      provider: queued.provider,
      model: queued.model ?? '',
      ...(queued.modelLabel ? {modelLabel: queued.modelLabel} : {}),
    }
  }
  const pin = threadModelOf(thread)
  if (pin) return pin
  const ai = useSettingsStore.getState().settings.ai
  return {provider: ai.activeProvider, model: ''}
}

/**
 * How the turn that ended on a thread went, from the message its stream committed: null when it answered, else the
 * reason the queue holds. A turn that left no message of its own (a failed or withdrawn send, a Stop before any text)
 * holds it too, as does an error bubble.
 */
function heldBy(messages: readonly ChatMessage[] | undefined, endedMessageId: string): 'error' | 'stopped' | null {
  const committed = messages?.find(m => m.id === endedMessageId)
  if (!committed) return 'error'
  if (committed.finishReason === 'error') return 'error'
  if (committed.finishReason === 'interrupted') return 'stopped'
  return null
}

export default function ChatStreamProvider({children}: {children: ReactNode}) {
  const engine = useChatStreamEngine()
  const {t} = useTranslation('chat')
  const toast = useOptionalToast()
  const notifyRef = useRef({t, toast})
  useEffect(() => {
    notifyRef.current = {t, toast}
  })

  useEffect(() => {
    let scheduled = false
    let disposed = false
    // The message id of the stream that last ended on each thread, until the drain has looked at how it went.
    const ended = new Map<string, string>()
    const drain = () => {
      scheduled = false
      if (disposed) return
      // A failed or stopped turn holds its thread's queue: the next follow-up would fail the same way, or the user
      // asked the chat to stop. The items stay, and the banner offers to send the next one.
      for (const [threadId, endedMessageId] of ended) {
        ended.delete(threadId)
        const now = useChatStore.getState()
        if (now.streams[threadId]) continue
        const reason = heldBy(now.conversations.get(threadId), endedMessageId)
        if (reason) now.pauseQueue(threadId, reason)
      }
      const st = useChatStore.getState()
      // One message per idle thread per pass: its send starts a stream, and the next waits for that stream's end.
      // Queued follow-ups are memory only (lost on reload) and go with the thread's context as it is now.
      for (const threadId of Object.keys(st.queuedByThread)) {
        const now = useChatStore.getState()
        const thread = now.threads.find(t => t.id === threadId)
        if (now.streams[threadId] || !thread || now.queuePausedByThread[threadId]) continue
        if (!now.canStartStream()) return
        const queued = now.takeQueuedMessage(threadId)
        if (!queued) continue
        const accepted = engine.send(threadId, queued.text, {
          ...queuedPair(queued, thread),
          pinThread: queued.pinThread,
          pinnedAs: queued.pinnedAs,
          files: queued.files.length > 0 ? queued.files : undefined,
          excludeContext: queued.excludeContext,
        })
        if (!accepted) {
          const after = useChatStore.getState()
          after.requeueMessage(threadId, queued)
          after.pauseQueue(threadId, 'refused')
          const {t: tr, toast: notify} = notifyRef.current
          notify?.warning(tr('composer.queueRefusedTitle'), {description: tr('composer.queueRefusedBody')})
        }
      }
    }
    const unsubscribe = useChatStore.subscribe((state, prev) => {
      if (
        state.streams === prev.streams &&
        state.queuedByThread === prev.queuedByThread &&
        state.queuePausedByThread === prev.queuePausedByThread
      ) {
        return
      }
      for (const [threadId, slot] of Object.entries(prev.streams)) {
        if (!state.streams[threadId]) ended.set(threadId, slot.messageId)
      }
      if (scheduled) return
      scheduled = true
      queueMicrotask(drain)
    })
    return () => {
      disposed = true
      unsubscribe()
    }
  }, [engine])

  return <ChatStreamContext.Provider value={engine}>{children}</ChatStreamContext.Provider>
}
