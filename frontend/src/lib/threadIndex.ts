import {chatApi} from '@/api'
import type {ServerThread} from '@/api/chat'
import {useChatStore, MAX_THREADS} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import type {ChatState} from '@/stores/chat/state'
import {nextHistoryToken} from '@/stores/chat/history'
import {persistFlowThreads} from '@/stores/chat/persistence'
import {serverThreadIndexAvailable} from '@/stores/chat/serverIndex'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import {logger} from '@/lib/logger'
import {uuid} from '@/lib/uuid'

// The server's thread index (R-frontend-c26 c): every thread a user stored on a flow is listed for them, so any
// browser can rebuild its list. The browser's own index (localStorage) stays the source for what was never stored,
// and the only one where the server keeps none (the desktop app, a deployment without accounts).

const PAGE_SIZE = 50

// Titles this client knows the server holds, by thread id: a title the index listed, or one stored by a send. A
// thread in this map also has a row on the server (listed, or made by naming it).
const serverTitles = new Map<string, string>()
// Local names not stored yet: a rename waiting for its send, or one whose send failed. They win over the index.
const dirtyTitles = new Set<string>()
const titleTimers = new Map<string, ReturnType<typeof setTimeout>>()
const titleSends = new Map<string, Promise<void>>()
// Threads a sync brought in: while nothing touches them they make way for another flow's threads.
const syncedThreads = new Set<string>()

/** For tests: forgets every title and thread the index has seen. */
export function resetThreadIndexForTest(): void {
  for (const timer of titleTimers.values()) clearTimeout(timer)
  titleTimers.clear()
  titleSends.clear()
  serverTitles.clear()
  dirtyTitles.clear()
  syncedThreads.clear()
}

/** Whether this client should read and write the server's thread index now. */
function serverIndexed(): boolean {
  return serverThreadIndexAvailable() && conversationHistoryEnabled()
}

function threadFromServer(flowId: string, s: ServerThread): ChatThread {
  return {
    id: s.id,
    flowId,
    title: s.title,
    createdAt: s.createdAt || s.updatedAt || new Date().toISOString(),
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
  }
}

// A thread the sync brought in that the user has not touched in this session: never opened (no history load), not
// active, nothing staged, drafted or streaming. Dropping it loses nothing: the server lists it.
function untouchedSynced(state: ChatState, t: ChatThread): boolean {
  return (
    syncedThreads.has(t.id) &&
    t.id !== state.activeThreadId &&
    state.historyStatus[t.id] === undefined &&
    !state.streams[t.id] &&
    !state.drafts[t.id] &&
    state.stagedPrompt?.threadId !== t.id
  )
}

/**
 * Merges the server's list of the user's threads of a flow into the client's index, without blocking the chat:
 * - a thread the browser lacks is added with its title, newest first, up to MAX_THREADS for the flow; to make room
 *   it drops other flows' threads a sync brought in and nothing touched (they stay listed on the server and come
 *   back with their flow), never a thread the user worked in;
 * - a thread the browser has takes the server's title, unless the user renamed it and that name is not stored yet
 *   (it is sent); a name the browser kept from before the index, for a thread the server lists unnamed, is sent;
 * - a local thread the server does not list is kept: it was never stored, or its conversation was cleared elsewhere
 *   while the thread lives on in that tab, and its next history load shows what the server holds;
 * - the active thread stays active: what is being typed, a staged prompt or a picked block belong to it.
 * Nothing happens where the server keeps no index, or with history saving off; a failed list changes nothing.
 */
export async function syncThreadIndex(flowId: string): Promise<void> {
  if (!serverIndexed()) return
  const listed: ServerThread[] = []
  try {
    let cursor: string | undefined
    do {
      const page = await chatApi.listThreads(flowId, {limit: PAGE_SIZE, ...(cursor ? {cursor} : {})})
      listed.push(...page.items)
      cursor = page.nextCursor
    } while (cursor && listed.length < MAX_THREADS)
  } catch (err) {
    logger.warn('Could not load the thread list', err)
    return
  }
  for (const s of listed) {
    if (!dirtyTitles.has(s.id)) serverTitles.set(s.id, s.title)
  }
  const listedTitles = new Map(listed.map(s => [s.id, s.title]))
  const toSend: string[] = []
  const evictedFlows = new Set<string>()

  useChatStore.setState(state => {
    const known = new Set(state.threads.map(t => t.id))
    const ownCount = state.threads.filter(t => t.flowId === flowId).length
    const wanted = listed.filter(s => !known.has(s.id)).slice(0, Math.max(0, MAX_THREADS - ownCount))
    // Room beside the threads in memory, made by dropping other flows' untouched synced threads, oldest first.
    const shortfall = wanted.length - (MAX_THREADS - state.threads.length)
    const drop = new Set<string>()
    if (shortfall > 0) {
      for (const t of state.threads) {
        if (drop.size >= shortfall) break
        if (t.flowId !== flowId && untouchedSynced(state, t)) {
          drop.add(t.id)
          evictedFlows.add(t.flowId)
        }
      }
    }
    const added = wanted.slice(0, Math.max(0, MAX_THREADS - state.threads.length + drop.size))
    for (const s of added) syncedThreads.add(s.id)
    for (const id of drop) syncedThreads.delete(id)

    let threads = state.threads
      .filter(t => !drop.has(t.id))
      .map(t => {
        if (t.flowId !== flowId) return t
        const server = listedTitles.get(t.id)
        if (dirtyTitles.has(t.id)) {
          toSend.push(t.id)
          return t
        }
        if (server === undefined) return t
        if (server) return server !== t.title ? {...t, title: server} : t
        // Listed unnamed while the browser has a name (kept from before the index): the browser's name is sent.
        if (t.title) toSend.push(t.id)
        return t
      })
    // Server order is newest first; the client's list grows at its end, oldest first.
    threads = [...threads, ...added.map(s => threadFromServer(flowId, s)).reverse()]
    if (drop.size === 0) return {threads}
    const conversations = new Map(state.conversations)
    for (const id of drop) conversations.delete(id)
    return {threads, conversations}
  })
  const threads = useChatStore.getState().threads
  persistFlowThreads(flowId, threads)
  for (const other of evictedFlows) persistFlowThreads(other, threads)
  for (const id of toSend) {
    dirtyTitles.add(id)
    scheduleTitle(id)
  }
}

/**
 * Moves a thread to a new key with nothing stored under it (CONVERSATION_KEY_TAKEN: its key holds another user's
 * conversation, R-security-3), keeping everything the client has for it — messages, draft, queue, place, name — so
 * its next save stores it under the new key at version 0, and its name follows to the index. Returns the new id,
 * or null when the thread is unknown or streaming (a running stream belongs to the old key: rekeyThreadWhenIdle).
 */
export function rekeyThread(oldId: string, reason: RekeyReason = 'keyTaken'): string | null {
  const st = useChatStore.getState()
  const thread = st.threads.find(t => t.id === oldId)
  if (!thread || st.streams[oldId]) return null
  const newId = uuid()
  useChatStore.setState(state => {
    const moved: Record<string, unknown> = {}
    // Every per-thread record keyed by the id (drafts, queues, refusals, …) follows the thread.
    for (const [field, value] of Object.entries(state)) {
      if (!value || typeof value !== 'object' || Array.isArray(value) || value instanceof Map) continue
      if (field === 'historyStatus' || field === 'streams' || field === 'lastActiveByFlow') continue
      const record = value as Record<string, unknown>
      if (!(oldId in record)) continue
      const next = {...record, [newId]: record[oldId]}
      delete next[oldId]
      moved[field] = next
    }
    const conversations = new Map(state.conversations)
    conversations.set(newId, conversations.get(oldId) ?? [])
    conversations.delete(oldId)
    const historyStatus = {
      ...state.historyStatus,
      [newId]: {state: 'loaded' as const, token: nextHistoryToken(), version: 0},
    }
    delete historyStatus[oldId]
    const lastActiveByFlow = Object.fromEntries(
      Object.entries(state.lastActiveByFlow).map(([flow, id]) => [flow, id === oldId ? newId : id]),
    )
    return {
      ...moved,
      threads: state.threads.map(t => (t.id === oldId ? {...t, id: newId, inFlight: undefined} : t)),
      conversations,
      historyStatus,
      lastActiveByFlow,
      activeThreadId: state.activeThreadId === oldId ? newId : state.activeThreadId,
      ...(state.stagedPrompt?.threadId === oldId ? {stagedPrompt: {...state.stagedPrompt, threadId: newId}} : {}),
    }
  })
  persistFlowThreads(thread.flowId, useChatStore.getState().threads)
  forgetTitle(oldId)
  syncedThreads.delete(oldId)
  if (thread.title) {
    dirtyTitles.add(newId)
    scheduleTitle(newId)
  }
  for (const l of rekeyListeners) l(oldId, newId, reason)
  return newId
}

/**
 * rekeyThread, once the thread's stream (a follow-up that started before the save was refused) has ended; null when
 * the thread closes meanwhile.
 */
export function rekeyThreadWhenIdle(oldId: string): Promise<string | null> {
  const st = useChatStore.getState()
  if (!st.threads.some(t => t.id === oldId)) return Promise.resolve(null)
  if (!st.streams[oldId]) return Promise.resolve(rekeyThread(oldId))
  return new Promise(resolve => {
    const unsubscribe = useChatStore.subscribe(state => {
      if (!state.threads.some(t => t.id === oldId)) {
        unsubscribe()
        resolve(null)
      } else if (!state.streams[oldId]) {
        unsubscribe()
        resolve(rekeyThread(oldId))
      }
    })
  })
}

/**
 * Why a thread moved to a new key: its key held another user's conversation (keyTaken), or the user cleared a
 * conversation stored before owners that they may not delete, so the thread starts afresh beside it (unowned).
 */
export type RekeyReason = 'keyTaken' | 'unowned'
type RekeyListener = (oldId: string, newId: string, reason: RekeyReason) => void
const rekeyListeners = new Set<RekeyListener>()

/** Calls cb whenever a thread is moved to a new key; returns the unsubscribe function. */
export function onThreadRekeyed(cb: RekeyListener): () => void {
  rekeyListeners.add(cb)
  return () => {
    rekeyListeners.delete(cb)
  }
}

function forgetTitle(threadId: string): void {
  const timer = titleTimers.get(threadId)
  if (timer) clearTimeout(timer)
  titleTimers.delete(threadId)
  serverTitles.delete(threadId)
  dirtyTitles.delete(threadId)
}

/**
 * The thread's conversation was cleared on the server, its name with it: the name is sent again, so the thread keeps
 * it in every browser's list.
 */
export function noteConversationCleared(threadId: string): void {
  serverTitles.delete(threadId)
  const thread = useChatStore.getState().threads.find(t => t.id === threadId)
  if (!thread?.title) return
  dirtyTitles.add(threadId)
  scheduleTitle(threadId)
}

/**
 * Before a thread closes: a name not sent yet is dropped (no row is made for a thread about to go), one on its way
 * is waited for. True when the server may hold a row for the thread — the index listed it, or a name stored one —
 * which the close then deletes.
 */
export function settleThreadTitle(threadId: string): boolean | Promise<boolean> {
  const timer = titleTimers.get(threadId)
  if (timer) clearTimeout(timer)
  titleTimers.delete(threadId)
  dirtyTitles.delete(threadId)
  // Answered at once unless a name is on its way: a close then goes ahead without waiting a tick.
  const sending = titleSends.get(threadId)
  return sending ? sending.then(() => serverTitles.has(threadId)) : serverTitles.has(threadId)
}

export const TITLE_SYNC_DEBOUNCE_MS = 800

function scheduleTitle(threadId: string): void {
  const pending = titleTimers.get(threadId)
  if (pending) clearTimeout(pending)
  titleTimers.set(
    threadId,
    setTimeout(() => sendTitle(threadId), TITLE_SYNC_DEBOUNCE_MS),
  )
}

// Sends the thread's current name. A thread that stored nothing yet is stored empty with its name, at a version the
// answer carries, which the client adopts while it still holds 0. A failed send leaves the name to the next change
// or sync; a key another user holds, or a conversation stored before owners, keeps its name in the client only.
function sendTitle(threadId: string): void {
  titleTimers.delete(threadId)
  const thread = useChatStore.getState().threads.find(t => t.id === threadId)
  if (!thread || !serverIndexed()) return
  const title = thread.title
  if (serverTitles.get(threadId) === title) {
    dirtyTitles.delete(threadId)
    return
  }
  const send: Promise<void> = Promise.resolve()
    .then(() => chatApi.setThreadTitle(thread.flowId, threadId, title))
    .then(res => {
      serverTitles.set(threadId, title)
      const st = useChatStore.getState()
      if (st.threads.find(t => t.id === threadId)?.title === title) dirtyTitles.delete(threadId)
      if (res.version !== undefined && st.getConversationVersion(threadId) === 0) {
        st.setConversationVersion(threadId, res.version)
      }
    })
    .catch(err => {
      const code = typeof err === 'object' && err !== null ? (err as {code?: unknown}).code : undefined
      if (code === 'CONVERSATION_KEY_TAKEN' || code === 'CONVERSATION_UNOWNED') dirtyTitles.delete(threadId)
      logger.warn('Could not save the thread title', err)
    })
    .finally(() => {
      if (titleSends.get(threadId) === send) titleSends.delete(threadId)
    })
  titleSends.set(threadId, send)
}

let titleSyncUsers = 0
let stopTitleSync: (() => void) | null = null

/**
 * Watches the user's thread names and sends each change, once it has settled for TITLE_SYNC_DEBOUNCE_MS, to the
 * server's index, so every browser's list shows it (R-frontend-c26 c): renames and auto-titles alike. A name the
 * index itself brought in is not sent back. Reference-counted: returns the stop function.
 */
export function startTitleSync(): () => void {
  titleSyncUsers++
  if (!stopTitleSync) {
    const unsubscribe = useChatStore.subscribe((state, prev) => {
      if (state.threads === prev.threads) return
      const before = new Map(prev.threads.map(t => [t.id, t.title]))
      for (const t of state.threads) {
        const was = before.get(t.id)
        if (was === undefined || was === t.title) continue
        if (serverTitles.get(t.id) === t.title && !dirtyTitles.has(t.id)) continue
        dirtyTitles.add(t.id)
        scheduleTitle(t.id)
      }
    })
    stopTitleSync = () => {
      unsubscribe()
    }
  }
  let stopped = false
  return () => {
    if (stopped) return
    stopped = true
    if (--titleSyncUsers === 0 && stopTitleSync) {
      stopTitleSync()
      stopTitleSync = null
    }
  }
}
