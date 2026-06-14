// AI chat session, message, and request/response shapes.

import type {ProviderID} from './providers';

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  timestamp: string;
  contextBlockId?: string;
  contextSubflowId?: string;
  tokensIn?: number;
  tokensOut?: number;
  provider?: ProviderID;
  model?: string;
  finishReason?: 'stop' | 'interrupted' | 'error';
}

export interface ChatRequest {
  flowId: string;
  provider: string;
  model?: string;
  messages: ChatMessage[];
  userMessage: string;
  contextBlockId?: string;
  selectedSourceFiles?: string[];
  systemPrompt?: string;
  temperature?: number;
  maxTokens?: number;
  demoMode?: boolean;
  excludeContext?: boolean;
  useTools?: boolean;
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

export interface ConversationSummary {
  id: string
  flowId: string
  provider: ProviderID
  model: string
  messageCount: number
  createdAt: string
  lastMessageAt: string
}

export interface ContextPreview {
  systemPrompt: string;
  contextText: string;
  userMessage: string;
  estimatedTokens: number;
  contextLimit: number;
}

export interface ConversationFile {
  version: number;
  flowKey: string;
  scope: string;
  updatedAt: string;
  messages: ChatMessage[];
}
