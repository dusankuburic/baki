// AI chat session, message, and request/response shapes.

import type {ProviderID} from './providers'

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
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  status: string
  message?: string
  items?: FixItemSnapshot[]
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
  // servedModel is the model that actually answered when `model` is an alias
  // the provider resolves (an Azure AI Foundry deployment, `model-router`, an
  // `agent:*`); absent when the backend did not report one. The rates are the
  // USD per 1M tokens this turn was billed at, when known — they price the
  // message instead of the current model's listing.
  servedModel?: string
  inputCostPerM?: number
  outputCostPerM?: number
  finishReason?: 'stop' | 'interrupted' | 'error'
  toolCalls?: ToolCallRecord[]
  fixProposal?: FixProposalSnapshot
  fixProposals?: FixProposalSnapshot[]
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
