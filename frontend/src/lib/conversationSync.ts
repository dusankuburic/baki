import {chatApi} from '@/api'
import {useChatStore} from '@/stores/chatStore'
import {mergeHistory} from '@/stores/chat/history'
import {logger} from '@/lib/logger'
import {noteConversationCleared, rekeyThreadWhenIdle} from '@/lib/threadIndex'
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

/**
 * Conversations are owned (R-security-3). CONVERSATION_KEY_TAKEN: the key holds another user's conversation, so no
 * save under it ever succeeds — the thread moves to a new key. CONVERSATION_UNOWNED: a conversation stored before
 * owners, which only someone who may delete the flow can clear — the thread leaves this client's list instead.
 */
export const CONVERSATION_KEY_TAKEN = 'CONVERSATION_KEY_TAKEN'
export const CONVERSATION_UNOWNED = 'CONVERSATION_UNOWNED'

function hasCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as {code?: unknown}).code === code
}

export function isConversationKeyTaken(err: unknown): boolean {
  return hasCode(err, CONVERSATION_KEY_TAKEN)
}

export function isConversationUnowned(err: unknown): boolean {
  return hasCode(err, CONVERSATION_UNOWNED)
}

// What goes to the server: finished turns only, a failed turn's error bubble stays local.
/** The separator an error bubble puts between the answer's partial text and its error line. */
const ERROR_LINE_SEPARATOR = '\n\n---\n'

/**
 * Whether a message is stored. A bare error bubble is the client's report of a failure, not something the assistant
 * said, and stays out. A failed turn that left something behind — partial text before its error line, tool calls or
 * fix cards — is the record the server stores too (R-approvals-4); a save replaces the stored list, so leaving it
 * out would erase it. The model never reads the error line: the server drops it from the history it sends.
 */
export function isPersistable(m: ChatMessage): boolean {
  if (m.finishReason !== 'error') return true
  if ((m.fixProposals?.length ?? 0) > 0 || (m.toolCalls?.length ?? 0) > 0) return true
  const cut = m.content.lastIndexOf(ERROR_LINE_SEPARATOR)
  return cut > 0 && m.content.slice(0, cut).trim() !== ''
}

/**
 * What the model reads of a message the client sends as history, as the server's historyContent does for the
 * history it owns: a failed turn's partial text without its error line, "" when the turn left no text.
 */
export function historyContent(m: ChatMessage): string {
  if (m.finishReason !== 'error') return m.content
  const cut = m.content.lastIndexOf(ERROR_LINE_SEPARATOR)
  return cut >= 0 ? m.content.slice(0, cut) : ''
}

/**
 * The history a request carries when the client sends it (history saving off, a resend, the restart auto-resend):
 * the messages a save would store (isPersistable), each as the model reads it, without one that has nothing to say
 * (an empty stopped turn, a failed turn that left only tool calls or fix cards). Error bubbles stay UI-only.
 */
export function requestHistory(messages: readonly ChatMessage[]): ChatMessage[] {
  const out: ChatMessage[] = []
  for (const m of messages) {
    if (!isPersistable(m)) continue
    const content = historyContent(m)
    if (m.role === 'assistant' && content.trim() === '') continue
    out.push({id: m.id, role: m.role, content, timestamp: m.timestamp})
  }
  return out
}

function persistable(threadId: string): ChatMessage[] {
  return useChatStore.getState().getMessages(threadId).filter(isPersistable)
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
  const messages = merged.filter(isPersistable)
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

/**
 * Refreshes the thread's version after the done of a server-owned turn it could not adopt (R-docs-8): one read, no
 * save. The server stored the turn itself, so a client still holding only what the server holds is merely behind —
 * typically a reload mid-stream, whose load read the version after the question was stored — and takes the server's
 * version, so its next clear or compaction is not refused. When the server holds a message this client has never
 * seen, another writer did write: the held version stays, and the next write's conflict says so truthfully.
 */
export async function syncThreadVersion(threadId: string, flowId: string, key: string): Promise<void> {
  const held = useChatStore.getState().getConversationVersion(threadId)
  if (held === undefined) return
  const load = await chatApi.getConversation(flowId, key)
  const st = useChatStore.getState()
  if (load.version === undefined || !st.threads.some(t => t.id === threadId)) return
  // Moved on meanwhile (a save, a clear): that write's version is the newer truth.
  if (st.getConversationVersion(threadId) !== held) return
  // Another writer added (a message this client has not seen) or deleted (a message of this client's the server no
  // longer holds, as a clear in another tab): the held version stays, so the next write conflicts and says so —
  // adopting would let that write silently undo the deletion. A long thread the server trimmed lands here too, and
  // pays one truthful-looking conflict.
  const local = st.getMessages(threadId)
  const mine = new Set(local.map(m => m.id))
  const theirs = new Set(load.messages.map(m => m.id))
  if (load.messages.some(m => !mine.has(m.id))) return
  if (local.some(m => isPersistable(m) && !theirs.has(m.id))) return
  st.setConversationVersion(threadId, load.version)
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
      if (isConversationKeyTaken(err)) {
        // Another user's key: the thread moves to a new one (nothing stored there) and is saved under it — once a
        // stream still running on it (a follow-up started before this save was refused) has ended.
        const moved = await rekeyThreadWhenIdle(threadId)
        if (!moved) throw err
        return saveThreadConversation(moved, flowId, moved)
      }
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
    if (isConversationKeyTaken(err)) {
      const moved = await rekeyThreadWhenIdle(threadId)
      if (!moved) throw err
      const stored = await replaceThreadConversation(moved, flowId, moved, messages)
      if (stored) st().setThreadMessages(moved, messages)
      return stored
    }
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
    // The thread's name went with its row: it is sent again, so the thread keeps it in the server's index.
    noteConversationCleared(threadId)
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
