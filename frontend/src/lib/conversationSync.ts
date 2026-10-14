import {chatApi} from '@/api'
import {useChatStore} from '@/stores/chatStore'
import {mergeHistory} from '@/stores/chat/history'
import {logger} from '@/lib/logger'
import type {DoneMeta} from '@/lib/chatEvent'
import type {ChatMessage} from '@/types'

/** The machine code of the 409 a stale conversation save or clear gets (ADR 0031). */
export const CONVERSATION_CONFLICT = 'CONVERSATION_CONFLICT'

/**
 * Whether err is the server's "this conversation changed since you loaded it". Read off the error's shape, not its
 * class, so it holds for a test double and for any ApiError flavour the client builds a 409 into.
 */
export function isConversationConflict(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as {code?: unknown}).code === CONVERSATION_CONFLICT
}

// What goes to the server: finished turns only, a failed turn's error bubble stays local.
function persistable(threadId: string): ChatMessage[] {
  return useChatStore
    .getState()
    .getMessages(threadId)
    .filter(m => m.finishReason !== 'error')
}

// The api call with the base version only when there is one: a thread without one (a server that has no versions)
// saves as it always did, and a mock sees the original argument list.
function save(flowId: string, key: string, messages: ChatMessage[], base: number | undefined) {
  return base === undefined
    ? chatApi.saveConversation(flowId, key, messages)
    : chatApi.saveConversation(flowId, key, messages, base)
}

/**
 * Adopts the server's conversation after a conflict: the loaded list, merged with the messages only this client has,
 * and its version. Returns the merged persistable list and whether the server already holds every message in it.
 */
async function reload(
  threadId: string,
  flowId: string,
  key: string,
): Promise<{messages: ChatMessage[]; complete: boolean}> {
  const load = await chatApi.getConversation(flowId, key)
  const st = useChatStore.getState()
  const merged = mergeHistory(load.messages, st.getMessages(threadId))
  st.setThreadMessages(threadId, merged)
  if (load.version !== undefined) st.setConversationVersion(threadId, load.version)
  const onServer = new Set(load.messages.map(m => m.id))
  const messages = merged.filter(m => m.finishReason !== 'error')
  return {messages, complete: messages.every(m => onServer.has(m.id))}
}

/**
 * Moves the thread to the version a stream's terminal event names, and says whether it did. The server stored the
 * turn itself, so the client holds the stored version without a save or a reload; but only if it still held the
 * version the turn began from. A thread that moved on meanwhile (another tab, a reload) keeps its own version, so
 * its next write conflicts and reconciles instead of replacing what it has not seen.
 */
export function adoptStreamVersion(threadId: string, meta: DoneMeta | undefined): boolean {
  if (meta?.conversationVersion === undefined || meta.conversationBase === undefined) return false
  const st = useChatStore.getState()
  const held = st.getConversationVersion(threadId)
  if (held === undefined || held !== meta.conversationBase || meta.conversationVersion < held) return false
  st.setConversationVersion(threadId, meta.conversationVersion)
  return true
}

// Rounds a save spends on conflicts before it settles for the server's version: every lost round is another writer's
// progress, so a few cover a busy thread, and a stream's own answer write lands within one.
const SAVE_ROUNDS = 3

/**
 * Saves the thread's finished messages with the version they were built on and remembers the version the server
 * answers. A conflict is a normal outcome rather than an error (another tab, or the server's own write of this
 * turn): reload, merge by message id and save again on the new version, unless the server already has every message.
 * When the rounds run out the client has still adopted the server's latest version and keeps its unsaved messages in
 * the thread for the next save, so nothing is reported and the next clear or compaction is not made on a stale version.
 */
export async function saveThreadConversation(threadId: string, flowId: string, key: string): Promise<void> {
  const st = () => useChatStore.getState()
  let messages = persistable(threadId)
  for (let round = 1; ; round++) {
    try {
      const res = await save(flowId, key, messages, st().getConversationVersion(threadId))
      if (res?.version !== undefined) st().setConversationVersion(threadId, res.version)
      return
    } catch (err) {
      if (!isConversationConflict(err)) throw err
    }
    const reloaded = await reload(threadId, flowId, key)
    if (reloaded.complete) return
    if (round >= SAVE_ROUNDS) {
      logger.warn('Conversation save left to the next one: the thread kept changing underneath it', {threadId})
      return
    }
    messages = reloaded.messages
  }
}

/**
 * Replaces the stored conversation with messages (compaction) on the version the client holds, true when stored. A
 * conflict is not retried: the stored list changed elsewhere, so this client takes it and returns false for the
 * caller to say so.
 */
export async function replaceThreadConversation(
  threadId: string,
  flowId: string,
  key: string,
  messages: ChatMessage[],
): Promise<boolean> {
  const st = () => useChatStore.getState()
  try {
    const res = await save(flowId, key, messages, st().getConversationVersion(threadId))
    if (res?.version !== undefined) st().setConversationVersion(threadId, res.version)
    return true
  } catch (err) {
    if (!isConversationConflict(err)) throw err
  }
  await adoptServerConversation(threadId, flowId, key)
  return false
}

/** Clears the stored conversation on the version the client holds; false after a conflict, with the server's list adopted. */
export async function clearThreadConversation(threadId: string, flowId: string, key: string): Promise<boolean> {
  const base = useChatStore.getState().getConversationVersion(threadId)
  try {
    await (base === undefined ? chatApi.clearConversation(flowId, key) : chatApi.clearConversation(flowId, key, base))
    return true
  } catch (err) {
    if (!isConversationConflict(err)) throw err
  }
  await adoptServerConversation(threadId, flowId, key)
  return false
}

// The thread shows what the server holds, whatever this client had.
async function adoptServerConversation(threadId: string, flowId: string, key: string): Promise<void> {
  const load = await chatApi.getConversation(flowId, key)
  const st = useChatStore.getState()
  st.setThreadMessages(threadId, load.messages)
  if (load.version !== undefined) st.setConversationVersion(threadId, load.version)
}
