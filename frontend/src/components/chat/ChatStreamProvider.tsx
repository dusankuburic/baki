import {useEffect, type ReactNode} from 'react'
import {useChatStore, threadModelOf, type ChatThread, type QueuedMessage} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStreamEngine, type StreamMeta} from './hooks/useChatStreamEngine'
import {ChatStreamContext} from './chatStreamContext'

// queuedPair is the provider/model a queued follow-up is sent with: the pair
// its thread had when it was queued — a pick made since must not redirect a
// message the user already committed to. One queued without a pair (an older
// build's) takes its thread's pin, else the provider for new chats.
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

// ChatStreamProvider hosts the chat stream engine for the whole signed-in
// session. It is mounted by App around the shell — above the inspector, the
// mobile drawer and the pop-out panel — so replies keep generating (and
// commit) while the chat panel is collapsed, closed or re-parented.
//
// Import it by this path, never through the components/chat barrel: the
// barrel re-exports MessageBubble, which would drag react-markdown into the
// entry chunk (see vite.config.ts).
export default function ChatStreamProvider({children}: {children: ReactNode}) {
  const engine = useChatStreamEngine()

  // Queued follow-ups (U1.6), drained here for EVERY thread: a message typed
  // while its thread was streaming is sent as soon as that thread is idle —
  // even if the chat panel is hidden or another thread is on screen. The
  // panel-level edge trigger this replaces missed streams that ended while the
  // panel was unmounted, leaving the message queued forever.
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
        // Guard BEFORE take (F1.7): a blocked send must stay queued.
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
      // The engine's own cleanup (its hook runs before this effect, so its
      // cleanup runs first) ends every live slot, which fires the subscription
      // above: the drain it schedules must not send into the torn-down engine.
      disposed = true
      unsubscribe()
    }
  }, [engine])

  return <ChatStreamContext.Provider value={engine}>{children}</ChatStreamContext.Provider>
}
