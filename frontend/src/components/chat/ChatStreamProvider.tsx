import {useEffect, type ReactNode} from 'react'
import {useChatStore, threadModelOf, type ChatThread, type QueuedMessage} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStreamEngine, type StreamMeta} from './hooks/useChatStreamEngine'
import {ChatStreamContext} from './chatStreamContext'

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

export default function ChatStreamProvider({children}: {children: ReactNode}) {
  const engine = useChatStreamEngine()

  useEffect(() => {
    let scheduled = false
    let disposed = false
    const drain = () => {
      scheduled = false
      if (disposed) return
      const st = useChatStore.getState()
      for (const [threadId, queued] of Object.entries(st.queuedByThread)) {
        const now = useChatStore.getState()
        const thread = now.threads.find(t => t.id === threadId)
        if (now.streams[threadId] || !thread) continue
        if (!now.canStartStream()) return
        if (!now.takeQueuedMessage(threadId)) continue
        void engine.send(threadId, queued.text, {
          ...queuedPair(queued, thread),
          pinThread: queued.pinThread,
          pinnedAs: queued.pinnedAs,
          files: queued.files.length > 0 ? queued.files : undefined,
          excludeContext: queued.excludeContext,
        })
      }
    }
    const unsubscribe = useChatStore.subscribe((state, prev) => {
      if (state.streams === prev.streams && state.queuedByThread === prev.queuedByThread) return
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
