
import type {ProviderID} from './providers'
import type {ReasoningEffort} from './settings'
import type {BlockAnchor} from '@/lib/blockAnchor'

export interface ToolCallRecord {
  name: string
  label?: string
  ok: boolean
  durationMs?: number
  summary?: string
}

export interface FixItemSnapshot {
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  status: string
  message?: string
}

export interface FixProposalSnapshot {
  proposalId: string
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

export interface ApprovalItemSnapshot {
  id: string
  label: string
  summary: string
  status: string
  message?: string
}

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
  modelLabel?: string
  servedModel?: string
  finishReason?: 'stop' | 'interrupted' | 'error'
  errorCode?: string
  errorShared?: boolean
  toolCalls?: ToolCallRecord[]
  fixProposal?: FixProposalSnapshot
  fixProposals?: FixProposalSnapshot[]
  approvals?: ApprovalSnapshot[]
  /** How many earlier history messages the turn left out to fit the model's context window. */
  truncated?: number
}

export interface ChatRequest {
  flowId: string
  provider: string
  model?: string
  messages?: ChatMessage[]
  userMessage: string
  contextBlockId?: string
  contextBlockAnchor?: BlockAnchor
  selectedSourceFiles?: string[]
  systemPrompt?: string
  temperature?: number
  maxTokens?: number
  reasoningEffort?: ReasoningEffort
  excludeContext?: boolean
  useTools?: boolean
  clientStreamId?: string
  conversationId?: string
  userMessageId?: string
  assistantMessageId?: string
}

export interface TokenUsage {
  promptTokens: number
  completionTokens: number
  totalTokens: number
}

export interface ContextPreviewBreakdown {
  system: number
  context: number
  history: number
  tools: number
  user: number
}

/** What the turn would send, built by the turn's own request assembly: the estimate counts every part the window check counts. */
export interface ContextPreview {
  systemPrompt: string
  contextText: string
  userMessage: string
  estimatedTokens: number
  contextLimit: number
  breakdown: ContextPreviewBreakdown
  historyMessages: number
  historyDropped?: number
  toolsOffered: boolean
  tools?: string[]
  contextBudget: number
  contextBlock?: 'resolved' | 'missing'
  contextBlockId?: string
}
