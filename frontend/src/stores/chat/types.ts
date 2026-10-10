import type {ProviderID, ToolCallRecord} from '@/types'

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

// The model-pin fields of a thread, as readThreadPin/setThreadModel handle
// them (persistence.ts and threads.ts).
export type ThreadPinFields = Pick<ChatThread, 'provider' | 'model' | 'modelLabel'>

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

// ApprovalItem is one item inside a (possibly batch) approval request, with
// its own resolved outcome after the batch decision lands. A fix carries the
// rule and the block it patches; an item of another kind has only the
// generic fields.
export interface ApprovalItem {
  // The item's key on the wire — the id the server stamped (a fix's
  // blockId:ruleId), or its ruleId from a server before that — by which a
  // decision's per-item outcomes are matched (helpers decisionTargets).
  id: string
  // What the item is about (a fix's block label); the summary is the
  // server-rendered preview text.
  label: string
  summary: string
  // pending → applied | applied-unresolved | error | already-resolved.
  status: 'pending' | 'applied' | 'applied-unresolved' | 'error' | 'already-resolved'
  message?: string
  // fix-patch only.
  ruleId?: string
  fixType?: string
  blockLabel?: string
  line?: number
}

// ApprovalCard is one approval request on a thread's stream slot (single or
// batch). Lives on the StreamSlot (transient by design): cards appear when
// the model calls a tool that needs approval, resolve when the user decides
// / the backend reports the outcome, and disappear with the slot when the
// stream ends — the committed message carries the persisted snapshots.
export interface ApprovalCard {
  proposalId: string
  // The tool awaiting approval ('' from a server that did not say) and how
  // its request is previewed: 'fix-patch' (apply_fix / apply_fixes, the
  // default for a server that sends no kind); any other kind renders the
  // generic preview.
  tool: string
  previewKind: string
  // Which event carried the request, and so which route takes the decision:
  // 'fix' (fix_proposal → /api/chat/fix-decision) or 'approval'
  // (approval_request → /api/chat/approval-decision).
  source: 'fix' | 'approval'
  // The actor scope the tool needs (shown as a hint when sent).
  requiredScope?: string
  // pending → applying (approved, mutation in flight) → applied |
  // applied-unresolved | declined | timeout | error | cancelled (the stream
  // ended before a decision). 'pending' is the only state with enabled
  // buttons.
  status: 'pending' | 'applying' | 'applied' | 'applied-unresolved' | 'declined' | 'timeout' | 'error' | 'cancelled'
  message?: string
  // items carries per-item state; single-fix prompts have exactly one item
  // and ALSO mirror it on the flat fields below (rendering + legacy
  // persistence).
  items: ApprovalItem[]
  // Flat mirrors of items[0] for single-fix prompts (empty for pure batches
  // and for other kinds).
  ruleId?: string
  fixType?: string
  blockLabel?: string
  line?: number
  summary?: string
  // When the request first reached this client (ms epoch). The approval
  // countdown derives from it, so a card that remounts (thread switch,
  // pop-out) keeps counting down instead of restarting at the full window
  // while the server's timer runs on. Stamped by setFixProposal.
  receivedAt?: number
}

/** The pre-4b names: a fix was the only approval. */
export type FixProposalItem = ApprovalItem
export type FixProposalCard = ApprovalCard

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
  // approvals stacks this stream's approval requests: a stream can carry
  // several sequential ones (fix → continue → next fix), and each batch
  // request is one card with per-item rows.
  approvals: ApprovalCard[]
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

export interface HistoryStatus {
  state: HistoryState
  // Generation token: a load that resolves after the thread was cleared,
  // reloaded or closed carries a stale token and is dropped.
  token: number
}
