import {create} from 'zustand'
import {registerStoreReset, currentUserId} from './storeRegistry'
import {chatApi} from '@/api'
import {uuid} from '@/lib/uuid'
import {storableModelLabel} from '@/lib/modelIds'
import {isTauri} from '@/platform/guards'
import {PROVIDERS} from '@/types'
import type {ChatMessage, ProviderID, ToolCallRecord} from '@/types'

// Mirrors the backend per-caller concurrency cap in internal/service/chat.go
// (maxConcurrentStreamsPerScope). The backend is authoritative; this constant
// lets the client give immediate feedback (disable Send + tooltip) instead of
// waiting for a rejected POST. Keep the two in sync.
export const MAX_CONCURRENT_STREAMS = 3

// MAX_THREADS bounds the in-memory conversation history; when exceeded, the
// oldest inactive, non-streaming thread is evicted (messages re-load from the
// backend on demand).
export const MAX_THREADS = 50

// conversationKey is the server-side key under which a thread's conversation
// is stored (chatApi.getConversation/saveConversation/clearConversation "scope"
// argument and ChatRequest.conversationId). One key per thread: the old key
// was contextBlockId-or-'flow', so every flow-scoped thread on a flow shared a
// single stored conversation and overwrote each other's history.
export function conversationKey(thread: Pick<ChatThread, 'id'>): string {
  return thread.id
}

// LEGACY_FLOW_SCOPE is the pre-thread conversation key. A flow whose threads
// were never persisted (first open after upgrade, or a different browser)
// restores its history from here once, then re-saves it under the new
// thread's own key.
export const LEGACY_FLOW_SCOPE = 'flow'

// Thread lists persist per flow in localStorage so a reload restores the same
// threads — and therefore the same conversation keys. Without this every
// reload minted a fresh thread id and the previous thread's history became
// unreachable. Conversations themselves stay server-side; only the thread
// metadata is stored here.
//
// Keys are per USER in the web app: v2:<userId>:<flowId>. The v1 key was
// per flow only, so on a shared browser the next account to open the same flow
// hydrated the previous account's thread ids (and titles, i.e. the start of
// each first prompt) — and the server lets any flow viewer read a
// conversation by key. The desktop app is single-user and keeps its v1 key.
const THREADS_KEY_V1 = 'baki:chatThreads:v1:'
const THREADS_KEY_V2 = 'baki:chatThreads:v2:'
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// threadsStorageKey returns where a flow's thread list lives, or null when it
// must not be stored at all (web app with no signed-in user).
function threadsStorageKey(flowId: string): string | null {
  if (isTauri()) return THREADS_KEY_V1 + flowId
  const userId = currentUserId()
  return userId ? `${THREADS_KEY_V2}${userId}:${flowId}` : null
}

// purgeLegacyThreadKeys drops the unscoped v1 entries in the web app. They
// record no owner, so they cannot be handed to whoever signs in next; their
// conversations stay on the server. Runs on every thread-list read (a flow
// open — a key scan is cheap) and on logout.
function purgeLegacyThreadKeys(): void {
  if (isTauri()) return
  try {
    const stale: string[] = []
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (k && k.startsWith(THREADS_KEY_V1)) stale.push(k)
    }
    for (const k of stale) localStorage.removeItem(k)
  } catch {
    // Storage unavailable: nothing to purge.
  }
}

type StoredThread = Pick<
  ChatThread,
  | 'id'
  | 'flowId'
  | 'title'
  | 'createdAt'
  | 'contextBlockId'
  | 'selectedSourceFiles'
  | 'tokensIn'
  | 'tokensOut'
  | 'useTools'
  | 'usedTools'
  | 'appliedFixes'
  | 'provider'
  | 'model'
  | 'modelLabel'
  | 'inFlight'
>

export function isProviderId(value: unknown): value is ProviderID {
  return typeof value === 'string' && Object.prototype.hasOwnProperty.call(PROVIDERS, value)
}

// A stored pin's strings are capped: a registry id is "m/" plus 10
// characters and a display name at most 64 on the server, a catalogue id a
// model name — anything longer did not come from this app.
const MAX_PIN_MODEL_LENGTH = 256
const MAX_PIN_LABEL_LENGTH = 200

type ThreadPinFields = Pick<ChatThread, 'provider' | 'model' | 'modelLabel'>

// readThreadPin validates a stored thread's model pin. Anything off — an
// unknown provider, a missing or oversized model — loads the thread
// unpinned (as every thread from before per-thread memory loads), and
// lib/modelSelection resolves it; a bad label alone is just dropped (as is
// one that is only an id — see storableModelLabel).
function readThreadPin(t: Record<string, unknown>): ThreadPinFields {
  const {provider, model, modelLabel} = t
  if (!isProviderId(provider)) return {}
  if (typeof model !== 'string' || !model.trim() || model.length > MAX_PIN_MODEL_LENGTH) return {}
  const pin: ThreadPinFields = {provider, model}
  const label =
    typeof modelLabel === 'string' && modelLabel.length <= MAX_PIN_LABEL_LENGTH
      ? storableModelLabel(modelLabel, model)
      : undefined
  if (label) pin.modelLabel = label
  return pin
}

// readInFlight validates a stored thread's in-flight turn record. Anything
// off drops the record: a reload then re-attaches the stream unlabelled
// rather than labelled with a guess. Stream and message ids are the
// client's own uuids; the model may be '' (the server's default).
function readInFlight(raw: unknown): InFlightTurn | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const r = raw as Record<string, unknown>
  if (typeof r.streamId !== 'string' || !UUID_RE.test(r.streamId)) return undefined
  if (!isProviderId(r.provider)) return undefined
  if (typeof r.model !== 'string' || r.model.length > MAX_PIN_MODEL_LENGTH) return undefined
  const turn: InFlightTurn = {streamId: r.streamId, provider: r.provider, model: r.model}
  if (typeof r.messageId === 'string' && UUID_RE.test(r.messageId)) turn.messageId = r.messageId
  const label =
    typeof r.modelLabel === 'string' && r.modelLabel.length <= MAX_PIN_LABEL_LENGTH
      ? storableModelLabel(r.modelLabel, r.model)
      : undefined
  if (label) turn.modelLabel = label
  return turn
}

function persistFlowThreads(flowId: string, threads: ChatThread[]): void {
  const key = threadsStorageKey(flowId)
  if (!key) return
  const mine: StoredThread[] = threads
    .filter(t => t.flowId === flowId)
    .map(t => ({
      id: t.id,
      flowId: t.flowId,
      title: t.title,
      createdAt: t.createdAt,
      contextBlockId: t.contextBlockId,
      selectedSourceFiles: t.selectedSourceFiles,
      tokensIn: t.tokensIn,
      tokensOut: t.tokensOut,
      useTools: t.useTools,
      usedTools: t.usedTools,
      appliedFixes: t.appliedFixes,
      provider: t.provider,
      model: t.model,
      modelLabel: t.modelLabel,
      inFlight: t.inFlight,
    }))
  try {
    if (mine.length === 0) localStorage.removeItem(key)
    else localStorage.setItem(key, JSON.stringify(mine))
  } catch {
    // Storage may be unavailable (private mode, quota); threads then last the session.
  }
}

function readStoredThreads(flowId: string): ChatThread[] {
  purgeLegacyThreadKeys()
  const key = threadsStorageKey(flowId)
  if (!key) return []
  let parsed: unknown
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return []
    parsed = JSON.parse(raw)
  } catch {
    // Storage unavailable or corrupt entry: start with no threads.
    return []
  }
  if (!Array.isArray(parsed)) return []
  const out: ChatThread[] = []
  for (const item of parsed) {
    if (!item || typeof item !== 'object') continue
    const t = item as Record<string, unknown>
    if (typeof t.id !== 'string' || !UUID_RE.test(t.id) || t.flowId !== flowId) continue
    out.push({
      id: t.id,
      flowId,
      title: typeof t.title === 'string' ? t.title : '',
      createdAt: typeof t.createdAt === 'string' ? t.createdAt : new Date().toISOString(),
      contextBlockId: typeof t.contextBlockId === 'string' ? t.contextBlockId : null,
      selectedSourceFiles: Array.isArray(t.selectedSourceFiles)
        ? t.selectedSourceFiles.filter((f): f is string => typeof f === 'string')
        : [],
      tokensIn: typeof t.tokensIn === 'number' ? t.tokensIn : 0,
      tokensOut: typeof t.tokensOut === 'number' ? t.tokensOut : 0,
      useTools: t.useTools === true ? true : undefined,
      usedTools: t.usedTools === true ? true : undefined,
      appliedFixes: t.appliedFixes === true ? true : undefined,
      ...readThreadPin(t),
    })
    const inFlight = readInFlight(t.inFlight)
    if (inFlight) out[out.length - 1].inFlight = inFlight
    if (out.length >= MAX_THREADS) break
  }
  return out
}

// mergeHistory lays the server's copy under the messages on screen: loaded
// messages in their stored order, and each local-only message (an error
// bubble, a turn sent while the load was in flight) right after the loaded
// message that precedes it locally — not all shoved to the end, where an old
// error bubble became the "last reply" Retry acted on. Local-only messages
// with no loaded predecessor were sent while the load ran and are newer than
// everything stored, so they trail it.
function mergeHistory(loaded: ChatMessage[], local: readonly ChatMessage[]): ChatMessage[] {
  const loadedIds = new Set(loaded.map(m => m.id))
  const after = new Map<string, ChatMessage[]>()
  const trailing: ChatMessage[] = []
  let anchor: string | null = null
  for (const m of local) {
    if (loadedIds.has(m.id)) {
      anchor = m.id
      continue
    }
    if (anchor === null) {
      trailing.push(m)
      continue
    }
    const list = after.get(anchor) ?? []
    list.push(m)
    after.set(anchor, list)
  }
  const merged: ChatMessage[] = []
  for (const m of loaded) {
    merged.push(m)
    const tail = after.get(m.id)
    if (tail) merged.push(...tail)
  }
  merged.push(...trailing)
  return merged
}

// nextThreadOfFlow picks the newest thread of a flow, or null when it has
// none. The thread that replaces a closed active thread must come from the
// SAME flow — falling back to any remaining thread showed another flow's
// conversation under the open one.
function nextThreadOfFlow(threads: readonly ChatThread[], flowId: string | undefined): string | null {
  if (!flowId) return null
  for (let i = threads.length - 1; i >= 0; i--) {
    if (threads[i].flowId === flowId) return threads[i].id
  }
  return null
}

export interface ChatThread {
  id: string
  flowId: string
  title: string
  createdAt: string
  contextBlockId: string | null
  selectedSourceFiles: string[]
  tokensIn: number
  tokensOut: number
  // useTools opts this thread into the read-only tool/agent loop (default off).
  useTools?: boolean
  // usedTools/appliedFixes are set at commit when the stream actually ran
  // tools / landed an applied fix — thread-tab badges so a glance shows which
  // conversations were agentic.
  usedTools?: boolean
  appliedFixes?: boolean
  // The thread's own model (per-thread memory): pinned on its first accepted
  // send, on a header pick, or when a removal notice is dismissed — never by
  // a settings change. All three or none: an unpinned thread (new, or from
  // before per-thread memory) starts on the provider's "Default for new
  // chats", or on its last reply's pair (lib/modelSelection). modelLabel is
  // the model's friendly name when it was pinned (never just an id).
  provider?: ProviderID
  model?: string
  modelLabel?: string
  // The turn in flight, as it was sent (see InFlightTurn).
  inFlight?: InFlightTurn
}

/**
 * A thread's turn in flight as it was sent: its stream, the assistant
 * message id the server stores the answer under, and the model it went to.
 * Persisted with the thread list, so a reload mid-turn re-attaches the
 * stream labelled with the model that is actually answering — which is not
 * the thread's pin when the turn went to a stand-in (pinThread: false).
 * Cleared when the turn ends (endStream).
 */
export interface InFlightTurn {
  streamId: string
  messageId?: string
  provider: ProviderID
  model: string
  modelLabel?: string
}

/** A thread's model pin as setThreadModel takes it. */
export interface ThreadModel {
  provider: ProviderID
  model: string
  modelLabel?: string
}

/**
 * A thread's pin as a comparable key ('' when unpinned) — what a captured
 * turn (queued, previewed) remembers, so its send re-pins only a thread
 * nobody re-picked since.
 */
export function threadPinKey(thread: Pick<ChatThread, 'provider' | 'model'> | undefined): string {
  return thread?.provider && thread.model ? `${thread.provider}\u0000${thread.model}` : ''
}

/** The thread's pin, or null when it has none (an incomplete one counts as none). */
export function threadModelOf(thread: Pick<ChatThread, 'provider' | 'model' | 'modelLabel'>): ThreadModel | null {
  if (!thread.provider || !thread.model) return null
  return {
    provider: thread.provider,
    model: thread.model,
    ...(thread.modelLabel ? {modelLabel: thread.modelLabel} : {}),
  }
}

// ModelRefusal records that the server refused a send because the thread's
// model is not available (MODEL_NOT_AVAILABLE on the create request) — the
// message went back to the composer and the provider listing is being
// refetched. Once a listing at least as new as `epoch` has loaded, the panel
// raises the model notice; normally that listing no longer offers the model
// and lib/modelSelection's removal notice says so, and this record only
// covers a listing that still does. Session-only.
export interface ModelRefusal extends ThreadModel {
  epoch: number
}

// FixProposalItem is one fix inside a (possibly batch) approval prompt, with
// its own resolved outcome after the batch decision lands.
export interface FixProposalItem {
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  // pending → applied | applied-unresolved | error | already-resolved.
  status: 'pending' | 'applied' | 'applied-unresolved' | 'error' | 'already-resolved'
  message?: string
}

// FixProposalCard is one approval prompt on a thread's stream slot (single or
// batch). Lives on the StreamSlot (transient by design): cards appear when
// the model requests fix(es), resolve when the user decides / the backend
// reports the outcome, and disappear with the slot when the stream ends —
// the committed message carries the persisted snapshots.
export interface FixProposalCard {
  proposalId: string
  // pending → applying (approved, mutation in flight) → applied |
  // applied-unresolved | declined | timeout | error. 'pending' is the only
  // state with enabled buttons.
  status: 'pending' | 'applying' | 'applied' | 'applied-unresolved' | 'declined' | 'timeout' | 'error'
  message?: string
  // items carries per-fix state; single-fix prompts have exactly one item and
  // ALSO mirror it on the flat fields below (rendering + legacy persistence).
  items: FixProposalItem[]
  // Flat mirrors of items[0] for single-fix prompts (empty for pure batches).
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  // When the proposal first reached this client (ms epoch). The approval
  // countdown derives from it, so a card that remounts (thread switch,
  // pop-out) keeps counting down instead of restarting at the full window
  // while the server's timer runs on. Stamped by setFixProposal.
  receivedAt?: number
}

// StreamSlot is one thread's in-flight AI response. Streams are per-thread so
// several threads can generate in parallel; the thinking/tokens/tool state
// lives here (not in hook-local state) so a background thread's progress
// survives switching tabs away and back.
export interface StreamSlot {
  streamId: string
  messageId: string
  text: string
  isThinking: boolean
  tokens: number
  toolStatus: string | null
  // toolCalls accumulates the finished tool executions (tool_result events)
  // for the response being streamed; committed onto the assistant message
  // when the stream ends.
  toolCalls: ToolCallRecord[]
  // fixProposals stacks this stream's approval prompts: a stream can carry
  // several sequential proposals (fix → continue → next fix), and each batch
  // proposal is one card with per-item rows.
  fixProposals: FixProposalCard[]
}

// QueuedMessage is a follow-up composed while its thread was streaming, sent
// automatically once the thread is idle. It carries the thread's
// provider/model as they were when it was queued (and whether the send pins
// the thread to them) so the always-mounted stream host can send it even
// while the chat panel is hidden — and a pick made meanwhile does not
// redirect it.
export interface QueuedMessage {
  text: string
  files: string[]
  excludeContext?: boolean
  provider?: ProviderID
  model?: string
  modelLabel?: string
  pinThread?: boolean
  // The thread's pin when this was queued (threadPinKey): a pick made since
  // keeps the thread's pin (see SendOptions.pinnedAs).
  pinnedAs?: string
}

// HistoryState tracks a thread's server-history load. Only a 'loaded' thread
// may be full-replace saved (or compacted): saving one whose history never
// arrived would overwrite the server copy with just the turns on screen.
export type HistoryState = 'loading' | 'loaded' | 'error'
interface HistoryStatus {
  state: HistoryState
  // Generation token: a load that resolves after the thread was cleared,
  // reloaded or closed carries a stale token and is dropped.
  token: number
}
let historyTokenSeq = 0

interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  // streams maps threadId → its in-flight response; absent = not streaming.
  streams: Record<string, StreamSlot>
  // The provider for NEW chats, mirrored from settings' ai.activeProvider by
  // the Settings panels and the start-up restore. The chat itself no longer
  // reads it: each thread resolves its own pair (its pin, else the active
  // provider's "Default for new chats" — lib/modelSelection), and a fallback
  // never writes it.
  selectedProvider: ProviderID
  providerEpoch: number
  // modelPickerRequest bumps to ask the chat header's model picker to open
  // (a notice's "Choose model", an error bubble's "Choose another model");
  // the picker opens on a change it sees while mounted.
  modelPickerRequest: number
  openModelPicker: () => void
  // modelRefusals: per thread, the model the server last refused a send for
  // (see ModelRefusal). Cleared by the thread's next accepted send, a pick,
  // dismissing its notice, or closing it.
  modelRefusals: Record<string, ModelRefusal>
  setModelRefusal: (threadId: string, refusal: ModelRefusal | null) => void
  // stagedPrompt carries a grounded prompt (from "Explain/Fix with AI") into a
  // thread's composer for review before the user sends it — nothing auto-sends.
  // Consumed by ChatInput once its threadId is the active thread, then cleared.
  stagedPrompt: {threadId: string; text: string} | null
  // drafts holds each thread's unsent composer text, keyed by threadId, so a
  // half-typed message survives switching threads or closing/reopening the AI
  // tab. Session-only (never persisted to the backend conversation payload).
  drafts: Record<string, string>
  // historyStatus: per-thread server-history load state (see HistoryState).
  // Absent = never attempted (a thread hydrated from localStorage).
  historyStatus: Record<string, HistoryStatus>
  // beginHistoryLoad marks the thread 'loading' and returns the load's token.
  beginHistoryLoad: (threadId: string) => number
  // finishHistoryLoad applies a load result if its token is still current:
  // loaded messages are MERGED BEFORE anything the user sent meanwhile
  // (deduped by id); `null` records a failed load. Returns whether applied.
  finishHistoryLoad: (threadId: string, token: number, loaded: ChatMessage[] | null) => boolean
  // markHistoryLoaded declares the thread's local messages authoritative
  // (a new thread, or one just cleared) and invalidates any load in flight.
  markHistoryLoaded: (threadId: string) => void
  isHistorySynced: (threadId: string) => boolean

  getMessages: (threadId: string) => readonly ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  clearThreadMessages: (threadId: string) => void
  // setThreadMessages replaces a thread's messages wholesale — Compact applies
  // the exact list it just persisted, so screen and server agree.
  setThreadMessages: (threadId: string, messages: readonly ChatMessage[]) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  // Queued follow-up (U1.6): one per thread, sent automatically when the
  // thread's stream ends. Composing while streaming is the expected UX — the
  // queue replaces a dead textarea.
  queuedByThread: Record<string, QueuedMessage>
  queueMessage: (threadId: string, msg: QueuedMessage) => void
  clearQueuedMessage: (threadId: string) => void
  takeQueuedMessage: (threadId: string) => QueuedMessage | undefined
  // addToolCall appends a finished tool execution (tool_result event) to the
  // thread's live stream slot.
  addToolCall: (threadId: string, record: ToolCallRecord) => void
  endStream: (threadId: string) => void
  setStreamMeta: (threadId: string, patch: Partial<Pick<StreamSlot, 'isThinking' | 'tokens' | 'toolStatus'>>) => void
  // setFixProposal shows one approval card on the thread's stream slot —
  // appended unless a card with the same proposalId exists (journal replay is
  // idempotent). patchFixProposal merges a decision/outcome update into the
  // matching card (+ per-item patches for batches).
  setFixProposal: (threadId: string, card: FixProposalCard) => void
  patchFixProposal: (
    threadId: string,
    proposalId: string,
    patch: Partial<FixProposalCard>,
    itemPatches?: {ruleId: string; patch: Partial<FixProposalItem>}[],
  ) => void
  // replaceStreamTools / replaceStreamFixes wholesale-replace the slot's tool
  // trail / proposal cards from an authoritative journal replay on reconnect —
  // replace (not append) keeps double-replay idempotent.
  replaceStreamTools: (threadId: string, calls: ToolCallRecord[]) => void
  replaceStreamFixes: (threadId: string, cards: FixProposalCard[]) => void
  // updateStream patches text AND meta in one atomic set, halving the per-frame
  // subscriber notifications during streaming (the high-frequency RAF-coalesced
  // flush otherwise issues two set() calls — text + tokens — at 60fps).
  updateStream: (
    threadId: string,
    patch: Partial<Pick<StreamSlot, 'text' | 'isThinking' | 'tokens' | 'toolStatus'>>,
  ) => void
  // activeStreamCount is the number of threads currently generating. Used by
  // the client-side cap guard (see MAX_CONCURRENT_STREAMS).
  activeStreamCount: () => number
  canStartStream: () => boolean

  createThread: (flowId: string) => string
  // hydrateThreads restores a flow's persisted thread list (see
  // persistFlowThreads). Returns the flow's thread ids — the ones already in
  // the store when present, otherwise the restored ones (last one active),
  // otherwise []. Conversations are not loaded here; the active-thread effect
  // fetches each thread's history by its key on demand.
  hydrateThreads: (flowId: string) => string[]
  switchThread: (threadId: string) => void
  // lastActiveByFlow remembers each flow's most recently active thread
  // (session-only), so a flow that is re-opened or refreshed returns to the
  // thread the user was in rather than merely the newest one.
  lastActiveByFlow: Record<string, string>
  // threadForFlow picks the thread to show for a flow: its last active thread
  // if still open, else its newest thread, else null.
  threadForFlow: (flowId: string) => string | null
  closeThread: (threadId: string) => void
  updateThread: (threadId: string, patch: Partial<ChatThread>) => void
  // setThreadModel pins a thread to a model (null unpins it) and persists it
  // with the thread list. A no-op when the pin is unchanged, and for an
  // incomplete pin or an unknown provider id. A label that is only an id is
  // not kept.
  setThreadModel: (threadId: string, pin: {provider: string; model: string; modelLabel?: string} | null) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  setProvider: (p: ProviderID) => void
  bumpProviderEpoch: () => void
  setStagedPrompt: (p: {threadId: string; text: string} | null) => void
  setDraft: (threadId: string, text: string) => void
}

const EMPTY_ARRAY: readonly ChatMessage[] = Object.freeze([] as ChatMessage[])

export const useChatStore = create<ChatState>((set, get) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  streams: {},
  queuedByThread: {},
  selectedProvider: 'claude',
  providerEpoch: 0,
  modelPickerRequest: 0,
  modelRefusals: {},
  stagedPrompt: null,
  drafts: {},
  historyStatus: {},
  lastActiveByFlow: {},

  beginHistoryLoad: threadId => {
    const token = ++historyTokenSeq
    set(state => ({historyStatus: {...state.historyStatus, [threadId]: {state: 'loading', token}}}))
    return token
  },

  finishHistoryLoad: (threadId, token, loaded) => {
    const cur = get().historyStatus[threadId]
    if (!cur || cur.token !== token || !get().threads.some(t => t.id === threadId)) return false
    if (loaded === null) {
      set(state => ({historyStatus: {...state.historyStatus, [threadId]: {state: 'error', token}}}))
      return true
    }
    set(state => {
      const local = state.conversations.get(threadId) ?? []
      const next = new Map(state.conversations)
      next.set(threadId, mergeHistory(loaded, local))
      return {conversations: next, historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token}}}
    })
    return true
  },

  markHistoryLoaded: threadId =>
    set(state => ({
      historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token: ++historyTokenSeq}},
    })),

  isHistorySynced: threadId => get().historyStatus[threadId]?.state === 'loaded',

  getMessages: threadId => {
    // Defensive copy: without this, callers receive the live internal array
    // reference and an accidental in-place mutation (push/splice) would
    // corrupt the store's state without triggering reactivity. EMPTY_ARRAY
    // is already immutable so it's returned as-is for missing threads.
    const msgs = get().conversations.get(threadId)
    return msgs ? [...msgs] : EMPTY_ARRAY
  },

  appendMessage: (threadId, message) =>
    set(state => {
      const existing = state.conversations.get(threadId) ?? []
      // Dedup by message ID — prevents duplicate entries when a conversation
      // is loaded twice (e.g. rapid flow switching, StrictMode double-invoke).
      if (existing.some(m => m.id === message.id)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [...existing, message])

      if (message.role === 'user' && !state.threads.find(t => t.id === threadId)?.title) {
        const title = message.content.slice(0, 40).replace(/\n/g, ' ')
        const threads = state.threads.map(t => (t.id === threadId ? {...t, title} : t))
        return {conversations: next, threads}
      }
      return {conversations: next}
    }),

  removeMessage: (threadId, messageId) =>
    set(state => {
      const msgs = state.conversations.get(threadId)
      if (!msgs) return state
      const next = new Map(state.conversations)
      next.set(
        threadId,
        msgs.filter(m => m.id !== messageId),
      )
      return {conversations: next}
    }),

  // Clearing makes the (now empty) local thread authoritative: an empty list
  // is kept rather than deleted, and the history status moves to 'loaded' with
  // a fresh token so a load still in flight cannot resurrect the old turns.
  clearThreadMessages: threadId =>
    set(state => {
      // A thread closed while its clear request was in flight stays gone.
      if (!state.threads.some(t => t.id === threadId)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [])
      return {
        conversations: next,
        historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token: ++historyTokenSeq}},
      }
    }),

  setThreadMessages: (threadId, messages) =>
    set(state => {
      if (!state.threads.some(t => t.id === threadId)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [...messages])
      return {conversations: next}
    }),

  compactThread: (threadId, keepPairs) =>
    set(state => {
      const msgs = state.conversations.get(threadId)
      if (!msgs || msgs.length <= keepPairs * 2) return state
      const next = new Map(state.conversations)
      next.set(threadId, msgs.slice(-keepPairs * 2))
      return {conversations: next}
    }),

  updateStreamingMessage: (threadId, text) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, text}}}
    }),

  startStream: (threadId, streamId, messageId) =>
    set(state => ({
      streams: {
        ...state.streams,
        [threadId]: {
          streamId,
          messageId,
          text: '',
          isThinking: true,
          tokens: 0,
          toolStatus: null,
          toolCalls: [],
          fixProposals: [],
        },
      },
    })),

  // addToolCall appends one finished tool execution to the thread's stream
  // slot (the live view of the per-message tool trail).
  addToolCall: (threadId, record) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, toolCalls: [...slot.toolCalls, record]}}}
    }),

  replaceStreamTools: (threadId, calls) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, toolCalls: calls}}}
    }),

  replaceStreamFixes: (threadId, cards) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      // A journal replay rebuilds the cards; keep each known card's arrival
      // time so its countdown doesn't restart.
      const seen = new Map(slot.fixProposals.map(c => [c.proposalId, c.receivedAt]))
      const fixProposals = cards.map(c => ({...c, receivedAt: c.receivedAt ?? seen.get(c.proposalId) ?? Date.now()}))
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals}}}
    }),

  // The turn is over: its slot goes, and with it the thread's in-flight
  // record (persisted) — only the record of THIS stream, never a newer one.
  endStream: threadId => {
    const slot = get().streams[threadId]
    if (!slot) return
    const thread = get().threads.find(t => t.id === threadId)
    const endsTurn = !!thread?.inFlight && thread.inFlight.streamId === slot.streamId
    set(state => {
      const next = {...state.streams}
      delete next[threadId]
      if (!endsTurn) return {streams: next}
      return {streams: next, threads: state.threads.map(t => (t.id === threadId ? {...t, inFlight: undefined} : t))}
    })
    if (endsTurn && thread) persistFlowThreads(thread.flowId, get().threads)
  },

  queueMessage: (threadId, msg) => set(state => ({queuedByThread: {...state.queuedByThread, [threadId]: msg}})),
  clearQueuedMessage: threadId =>
    set(state => {
      if (!(threadId in state.queuedByThread)) return state
      const next = {...state.queuedByThread}
      delete next[threadId]
      return {queuedByThread: next}
    }),
  takeQueuedMessage: threadId => {
    const msg = get().queuedByThread[threadId]
    if (msg) get().clearQueuedMessage(threadId)
    return msg
  },

  // setFixProposal appends a card unless one with the same proposalId already
  // exists (replay idempotence) — sequential proposals stack, earlier
  // outcomes are kept.
  setFixProposal: (threadId, card) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      if (slot.fixProposals.some(c => c.proposalId === card.proposalId)) return state
      const stamped = {...card, receivedAt: card.receivedAt ?? Date.now()}
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals: [...slot.fixProposals, stamped]}}}
    }),

  // patchFixProposal merges a status/message update into the matching card
  // (a no-op when the card is gone — the stream may have ended) and applies
  // per-item patches (batch decisions) by ruleId.
  patchFixProposal: (threadId, proposalId, patch, itemPatches) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      const idx = slot.fixProposals.findIndex(c => c.proposalId === proposalId)
      if (idx < 0) return state
      const card = slot.fixProposals[idx]
      let items = card.items
      if (itemPatches && itemPatches.length > 0) {
        items = card.items.map(it => {
          const p = itemPatches.find(ip => ip.ruleId === it.ruleId)
          return p ? {...it, ...p.patch} : it
        })
      }
      const fixProposals = [...slot.fixProposals]
      fixProposals[idx] = {...card, ...patch, items}
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals}}}
    }),

  setStreamMeta: (threadId, patch) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
    }),

  updateStream: (threadId, patch) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
    }),

  activeStreamCount: () => Object.keys(get().streams).length,
  canStartStream: () => get().activeStreamCount() < MAX_CONCURRENT_STREAMS,

  createThread: flowId => {
    const id = uuid()
    const thread: ChatThread = {
      id,
      flowId,
      title: '',
      createdAt: new Date().toISOString(),
      contextBlockId: null,
      selectedSourceFiles: [],
      tokensIn: 0,
      tokensOut: 0,
    }
    set(state => {
      // Pre-populate with [] so the backend-restore effect treats this as
      // already initialised. Without this, every new thread would re-load
      // the previous thread's conversation from disk (same 'flow' scope).
      const nextConversations = new Map(state.conversations)
      nextConversations.set(id, [])
      // A brand-new thread has no server history to wait for.
      const historyStatus: Record<string, HistoryStatus> = {
        ...state.historyStatus,
        [id]: {state: 'loaded', token: ++historyTokenSeq},
      }
      let threads = [...state.threads, thread]
      // Evict the oldest inactive, non-streaming thread when the cap is
      // exceeded, so long sessions don't accumulate unbounded chat history.
      if (threads.length > MAX_THREADS) {
        const victim = threads.find(t => t.id !== id && t.id !== state.activeThreadId && !(t.id in state.streams))
        if (victim) {
          threads = threads.filter(t => t.id !== victim.id)
          nextConversations.delete(victim.id)
          const drafts = victim.id in state.drafts ? {...state.drafts} : state.drafts
          if (victim.id in drafts) delete drafts[victim.id]
          const queuedByThread = victim.id in state.queuedByThread ? {...state.queuedByThread} : state.queuedByThread
          if (victim.id in queuedByThread) delete queuedByThread[victim.id]
          const modelRefusals = victim.id in state.modelRefusals ? {...state.modelRefusals} : state.modelRefusals
          if (victim.id in modelRefusals) delete modelRefusals[victim.id]
          delete historyStatus[victim.id]
          return {
            threads,
            activeThreadId: id,
            conversations: nextConversations,
            drafts,
            queuedByThread,
            modelRefusals,
            historyStatus,
            lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: id},
          }
        }
      }
      return {
        threads: [...state.threads, thread],
        activeThreadId: id,
        conversations: nextConversations,
        historyStatus,
        lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: id},
      }
    })
    persistFlowThreads(flowId, get().threads)
    return id
  },

  hydrateThreads: flowId => {
    const existing = get().threads.filter(t => t.flowId === flowId)
    if (existing.length > 0) return existing.map(t => t.id)
    const stored = readStoredThreads(flowId)
    if (stored.length === 0) return []
    const active = stored[stored.length - 1].id
    set(state => ({
      threads: [...state.threads, ...stored],
      activeThreadId: active,
      lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: active},
    }))
    return stored.map(t => t.id)
  },

  switchThread: threadId => {
    const thread = get().threads.find(t => t.id === threadId)
    if (thread) {
      set(state => ({
        activeThreadId: threadId,
        lastActiveByFlow: {...state.lastActiveByFlow, [thread.flowId]: threadId},
      }))
    }
  },

  threadForFlow: flowId => {
    const {threads, lastActiveByFlow} = get()
    const last = lastActiveByFlow[flowId]
    if (last && threads.some(t => t.id === last && t.flowId === flowId)) return last
    for (let i = threads.length - 1; i >= 0; i--) {
      if (threads[i].flowId === flowId) return threads[i].id
    }
    return null
  },

  closeThread: threadId => {
    // Cancel any in-flight backend stream before clearing the local slot, or
    // the provider keeps generating for a stream with no listener.
    const slot = get().streams[threadId]
    if (slot?.streamId) {
      chatApi.cancelStream(slot.streamId).catch(() => {})
    }
    const closedFlowId = get().threads.find(t => t.id === threadId)?.flowId
    set(state => {
      const remaining = state.threads.filter(t => t.id !== threadId)
      const next = new Map(state.conversations)
      next.delete(threadId)
      let streams = state.streams
      if (threadId in streams) {
        streams = {...streams}
        delete streams[threadId]
      }
      const drafts = threadId in state.drafts ? {...state.drafts} : state.drafts
      if (threadId in drafts) delete drafts[threadId]
      // The queued follow-up dies with its thread, same as the draft — leaving
      // it behind kept an unsendable message in the store for the session.
      const queuedByThread = threadId in state.queuedByThread ? {...state.queuedByThread} : state.queuedByThread
      if (threadId in queuedByThread) delete queuedByThread[threadId]
      const modelRefusals = threadId in state.modelRefusals ? {...state.modelRefusals} : state.modelRefusals
      if (threadId in modelRefusals) delete modelRefusals[threadId]
      const historyStatus = {...state.historyStatus}
      delete historyStatus[threadId]
      // The replacement is the flow's own newest thread; none left ⇒ null,
      // and useChatConversations creates a fresh one for the flow.
      const activeThreadId =
        state.activeThreadId === threadId ? nextThreadOfFlow(remaining, closedFlowId) : state.activeThreadId
      return {
        threads: remaining,
        conversations: next,
        activeThreadId,
        streams,
        drafts,
        queuedByThread,
        modelRefusals,
        historyStatus,
      }
    })
    if (closedFlowId) persistFlowThreads(closedFlowId, get().threads)
  },

  updateThread: (threadId, patch) => {
    set(state => ({
      threads: state.threads.map(t => (t.id === threadId ? {...t, ...patch} : t)),
    }))
    const flowId = get().threads.find(t => t.id === threadId)?.flowId
    if (flowId) persistFlowThreads(flowId, get().threads)
  },

  setThreadModel: (threadId, pin) => {
    const thread = get().threads.find(t => t.id === threadId)
    if (!thread) return
    let next: ThreadPinFields
    if (pin === null) {
      next = {provider: undefined, model: undefined, modelLabel: undefined}
    } else {
      if (!isProviderId(pin.provider) || !pin.model) return
      // Only a name is stored as the label, never an id (storableModelLabel).
      next = {provider: pin.provider, model: pin.model, modelLabel: storableModelLabel(pin.modelLabel, pin.model)}
    }
    if (thread.provider === next.provider && thread.model === next.model && thread.modelLabel === next.modelLabel) {
      return
    }
    get().updateThread(threadId, next)
  },

  getActiveThread: () => {
    const {threads, activeThreadId} = get()
    return threads.find(t => t.id === activeThreadId) ?? null
  },

  getFlowThreads: flowId => {
    return get().threads.filter(t => t.flowId === flowId)
  },

  clearFlowThreads: flowId => {
    set(state => {
      const toRemove = state.threads.filter(t => t.flowId === flowId).map(t => t.id)
      const remaining = state.threads.filter(t => t.flowId !== flowId)
      const next = new Map(state.conversations)
      for (const id of toRemove) next.delete(id)
      // The flow keeps no thread to fall back to; another flow's must not be
      // shown in its place.
      const activeThreadId = toRemove.includes(state.activeThreadId ?? '') ? null : state.activeThreadId
      // Full per-thread cleanup (F1.7), mirroring closeThread: streams,
      // drafts, and queued follow-ups for removed threads must not linger
      // (a lingering stream would eat a concurrency-cap slot forever).
      const streams = {...state.streams}
      const drafts = {...state.drafts}
      const queuedByThread = {...state.queuedByThread}
      const modelRefusals = {...state.modelRefusals}
      const historyStatus = {...state.historyStatus}
      for (const id of toRemove) {
        delete streams[id]
        delete drafts[id]
        delete queuedByThread[id]
        delete modelRefusals[id]
        delete historyStatus[id]
      }
      return {
        threads: remaining,
        conversations: next,
        activeThreadId,
        streams,
        drafts,
        queuedByThread,
        modelRefusals,
        historyStatus,
      }
    })
    persistFlowThreads(flowId, get().threads)
  },

  setProvider: p => set({selectedProvider: p}),

  bumpProviderEpoch: () => set(s => ({providerEpoch: s.providerEpoch + 1})),
  openModelPicker: () => set(s => ({modelPickerRequest: s.modelPickerRequest + 1})),
  setModelRefusal: (threadId, refusal) =>
    set(state => {
      if (refusal) return {modelRefusals: {...state.modelRefusals, [threadId]: refusal}}
      if (!(threadId in state.modelRefusals)) return state
      const modelRefusals = {...state.modelRefusals}
      delete modelRefusals[threadId]
      return {modelRefusals}
    }),
  setStagedPrompt: p => set({stagedPrompt: p}),

  setDraft: (threadId, text) =>
    set(state => {
      // Prune empty drafts so the map doesn't accumulate blank keys.
      if (!text) {
        if (!(threadId in state.drafts)) return state
        const drafts = {...state.drafts}
        delete drafts[threadId]
        return {drafts}
      }
      if (state.drafts[threadId] === text) return state
      return {drafts: {...state.drafts, [threadId]: text}}
    }),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => {
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    stagedPrompt: null,
    selectedProvider: 'claude',
    providerEpoch: 0,
    modelPickerRequest: 0,
    modelRefusals: {},
    drafts: {},
    // Queued (unsent) follow-ups are session state — they must not survive
    // logout (F1.7).
    queuedByThread: {},
    historyStatus: {},
    lastActiveByFlow: {},
  })
  // Unscoped legacy thread lists must not outlive the session on a shared
  // browser (per-user v2 lists stay — they only load for their owner).
  purgeLegacyThreadKeys()
})
