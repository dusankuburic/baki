import {createContext, useContext} from 'react'
import type {ChatStreamEngine} from './hooks/useChatStreamEngine'

// ChatStreamContext carries the app-wide chat stream engine (see
// ChatStreamProvider). Deliberately null by default: a chat panel rendered
// without the provider must fail loudly, because a silent fallback engine
// owned by the panel would quietly reintroduce "hiding the chat kills the
// reply".
export const ChatStreamContext = createContext<ChatStreamEngine | null>(null)

export function useChatStream(): ChatStreamEngine {
  const engine = useContext(ChatStreamContext)
  if (!engine) throw new Error('useChatStream must be used within ChatStreamProvider')
  return engine
}
