import {createContext, useContext} from 'react'
import type {ChatStreamEngine} from './hooks/useChatStreamEngine'

export const ChatStreamContext = createContext<ChatStreamEngine | null>(null)

export function useChatStream(): ChatStreamEngine {
  const engine = useContext(ChatStreamContext)
  if (!engine) throw new Error('useChatStream must be used within ChatStreamProvider')
  return engine
}
