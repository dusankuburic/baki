import {describe, it, expect, beforeEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useChatRequestBuilder} from './useChatRequestBuilder'
import {useChatStore} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import type {AISettings, ChatMessage, FlowDocument} from '@/types'

const doc = {id: 'flow1', name: 'Flow', filePath: '/f.txt', subflows: [], metadata: {blockCount: 0, subflowCount: 0, maxDepth: 0, parsedAt: '', fileSize: 0, rawLineCount: 0}} as FlowDocument

function makeThread(over: Partial<ChatThread> = {}): ChatThread {
  return {
    id: 't1', flowId: 'flow1', title: '', createdAt: '', contextBlockId: null,
    selectedSourceFiles: [], tokensIn: 0, tokensOut: 0, ...over,
  }
}

function makeAISettings(): AISettings {
  return {
    activeProvider: 'openai',
    embeddingProvider: 'openai',
    providers: {
      openai: {enabled: true, defaultModel: 'gpt-default', temperature: 0.3, maxTokens: 4096, contextTokenBudget: 8000},
    } as AISettings['providers'],
    demoMode: {enabled: false, dailyLimit: 0, dailyUsed: 0, resetDate: ''},
    showCostEstimates: true,
    saveConversationHistory: true,
    dailyBudget: 0,
    prompts: {} as AISettings['prompts'],
  }
}

const initialChatState = useChatStore.getState()

describe('useChatRequestBuilder', () => {
  beforeEach(() => {
    useChatStore.setState(initialChatState, true)
  })

  it('returns null when there is no document', () => {
    const {result} = renderHook(() => useChatRequestBuilder({
      doc: null, activeThread: makeThread(), provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    expect(result.current.buildRequest('hi')).toBeNull()
  })

  it('returns null when there is no active thread', () => {
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: null, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    expect(result.current.buildRequest('hi')).toBeNull()
  })

  it('builds a request with flowId, provider, model, and contextBlockId', () => {
    const thread = makeThread({contextBlockId: 'b1'})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    const req = result.current.buildRequest('hello')
    expect(req).toMatchObject({
      flowId: 'flow1', provider: 'openai', model: 'gpt-4', userMessage: 'hello',
      contextBlockId: 'b1', excludeContext: false, useTools: false,
    })
    expect(req).not.toHaveProperty('messages')
  })

  it('falls back to the provider default model when selectedModel is empty', () => {
    const thread = makeThread()
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: '',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    const req = result.current.buildRequest('hi')
    expect(req?.model).toBe('gpt-default')
  })

  it('includes messages only when includeHistory is true', () => {
    const thread = makeThread()
    useChatStore.setState({threads: [thread]})
    const msgs: ChatMessage[] = [{id: 'm1', role: 'user', content: 'hi', timestamp: 't'}]
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => msgs,
    }))
    expect(result.current.buildRequest('hi')).not.toHaveProperty('messages')
    const withHistory = result.current.buildRequest('hi', undefined, undefined, true)
    expect(withHistory?.messages).toEqual([{id: 'm1', role: 'user', content: 'hi', timestamp: 't'}])
  })

  it('uses the thread\'s stored selectedSourceFiles when overrideFiles is not given', () => {
    const thread = makeThread({selectedSourceFiles: ['a.txt', 'b.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    expect(result.current.buildRequest('hi')?.selectedSourceFiles).toEqual(['a.txt', 'b.txt'])
  })

  it('overrideFiles takes precedence over the thread\'s stored selection', () => {
    const thread = makeThread({selectedSourceFiles: ['a.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    expect(result.current.buildRequest('hi', ['c.txt'])?.selectedSourceFiles).toEqual(['c.txt'])
  })

  it('excludeContext clears selectedSourceFiles but keeps contextBlockId', () => {
    const thread = makeThread({contextBlockId: 'b1', selectedSourceFiles: ['a.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    const req = result.current.buildRequest('hi', undefined, true)
    expect(req?.selectedSourceFiles).toBeUndefined()
    expect(req?.excludeContext).toBe(true)
    expect(req?.contextBlockId).toBe('b1')
  })

  it('reflects the thread\'s useTools flag', () => {
    const thread = makeThread({useTools: true})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() => useChatRequestBuilder({
      doc, activeThread: thread, provider: 'openai', selectedModel: 'gpt-4',
      aiSettings: makeAISettings(), getMessages: () => [],
    }))
    expect(result.current.buildRequest('hi')?.useTools).toBe(true)
  })
})
