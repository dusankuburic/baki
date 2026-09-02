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
  finishReason?: 'stop' | 'interrupted' | 'error'
  toolCalls?: ToolCallRecord[]
  fixProposal?: FixProposalSnapshot
  fixProposals?: FixProposalSnapshot[]
}

export interface ChatRequest {
  flowId: string
  provider: string
  model?: string
  messages: ChatMessage[]
  userMessage: string
  contextBlockId?: string
  selectedSourceFiles?: string[]
  systemPrompt?: string
  temperature?: number
  maxTokens?: number
  demoMode?: boolean
  excludeContext?: boolean
  useTools?: boolean
  // C-1: client-generated stream ID. When set, the client subscribes its SSE
  // listener before POSTing create, so the backend emits immediately with no
  // /chat/begin round-trip. Omitted ⇒ legacy two-POST handshake.
  clientStreamId?: string
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

export interface ConversationFile {
  version: number
  flowKey: string
  scope: string
  updatedAt: string
  messages: ChatMessage[]
}
