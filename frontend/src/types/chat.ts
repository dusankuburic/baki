// AI chat session, message, and request/response shapes.

import type {ProviderID} from './providers'
import type {ReasoningEffort} from './settings'

// ToolCallRecord is one tool invocation behind an assistant message — the
// transparency trail (mirrors the backend's tool_result wire event).
export interface ToolCallRecord {
  name: string
  label?: string
  ok: boolean
  durationMs?: number
  summary?: string
}

// FixItemSnapshot is one fix inside a (batch) persisted approval record, with
// its own resolved outcome.
export interface FixItemSnapshot {
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  status: string
  message?: string
}

// FixProposalSnapshot is the persisted apply_fix approval record: what was
// proposed and how it resolved. Attached to the assistant message so the
// decision outlives the stream's transient approval card.
export interface FixProposalSnapshot {
  proposalId: string
  // The tool behind the proposal and how it was previewed, as the server
  // stamps them; absent on older records, which were always a fix patch.
  tool?: string
  previewKind?: string
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  status: string
  message?: string
  items?: FixItemSnapshot[]
}

// ApprovalItemSnapshot is one item of a persisted approval of another kind:
// its label and summary are the server's rendering, kept as text.
export interface ApprovalItemSnapshot {
  id: string
  label: string
  summary: string
  status: string
  message?: string
}

// ApprovalSnapshot is the persisted record of an approval that was not a fix
// (a later server's message.approvals[]): the tool, how it was previewed and
// how it resolved. Read as sent and re-saved as read; this bundle never
// writes one itself — the server keeps the record.
export interface ApprovalSnapshot {
  proposalId: string
  tool?: string
  previewKind?: string
  requiredScope?: string
  status: string
  message?: string
  items: ApprovalItemSnapshot[]
}

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
  contextBlockId?: string
  tokensIn?: number
  tokensOut?: number
  provider?: ProviderID
  model?: string
  // modelLabel is the model's friendly name when the message was sent (an
  // Azure AI Foundry registry entry's display name for an "m/<id>" model),
  // kept end to end so history and exports still name a model that was later
  // renamed or removed.
  modelLabel?: string
  // servedModel is the model that actually answered when `model` is an alias
  // the provider resolves (an Azure AI Foundry deployment, `model-router`, an
  // `agent:*`); absent when the backend did not report one. The rates are the
  // USD per 1M tokens this turn was billed at, when known — they price the
  // message instead of the current model's listing.
  servedModel?: string
  inputCostPerM?: number
  outputCostPerM?: number
  finishReason?: 'stop' | 'interrupted' | 'error'
  // An error bubble's machine-readable failure class (PROVIDER_AUTH, …), when
  // the backend sent one; drives the bubble's "Open settings" action. Error
  // bubbles are never persisted, so this never reaches the server.
  errorCode?: string
  // True when the failure came from the organisation's shared credential:
  // the bubble then also offers the Organizations settings section,
  // where an admin — not the member — can fix it. Never persisted.
  errorShared?: boolean
  toolCalls?: ToolCallRecord[]
  fixProposal?: FixProposalSnapshot
  fixProposals?: FixProposalSnapshot[]
  approvals?: ApprovalSnapshot[]
}

export interface ChatRequest {
  flowId: string
  provider: string
  model?: string
  // Omitted on the normal path: the backend replays its stored conversation.
  // Sent only when the client's (locally truncated) history must win —
  // resend/regenerate — or when history saving is off.
  messages?: ChatMessage[]
  userMessage: string
  contextBlockId?: string
  selectedSourceFiles?: string[]
  systemPrompt?: string
  temperature?: number
  maxTokens?: number
  // The provider's reasoning depth; omitted keeps the provider default.
  reasoningEffort?: ReasoningEffort
  excludeContext?: boolean
  useTools?: boolean
  // C-1: client-generated stream ID. When set, the client subscribes its SSE
  // listener before POSTing create, so the backend emits immediately with no
  // /chat/begin round-trip. Omitted ⇒ legacy two-POST handshake.
  clientStreamId?: string
  // The client thread id. It is the server-side conversation key, so two
  // threads on the same flow keep separate histories (see ChatRequest
  // .ConversationKey on the backend).
  conversationId?: string
  // The client's ids for this turn's two messages. The server stores its own
  // copies of the turn under the same ids, so a reloaded conversation merged
  // with the messages already on screen dedupes by id instead of doubling.
  userMessageId?: string
  assistantMessageId?: string
}

export interface ChatResponse {
  message: ChatMessage
  usage: TokenUsage
  durationMs: number
}

export interface TokenUsage {
  promptTokens: number
  completionTokens: number
  totalTokens: number
}

export interface ContextPreview {
  systemPrompt: string
  contextText: string
  userMessage: string
  estimatedTokens: number
  contextLimit: number
}
