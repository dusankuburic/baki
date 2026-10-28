import {describe, it, expect, expectTypeOf, beforeEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {buildChatRequest, threadUsesTools, useChatRequestBuilder} from './useChatRequestBuilder'
import {isPersistable} from '@/lib/conversationSync'
import {useChatStore} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'
import type {AISettings, ChatMessage, ChatRequest, FlowDocument} from '@/types'

const doc = {
  id: 'flow1',
  name: 'Flow',
  filePath: '/f.txt',
  subflows: [],
  metadata: {blockCount: 0, subflowCount: 0, maxDepth: 0, parsedAt: '', fileSize: 0, rawLineCount: 0},
} as FlowDocument

function makeThread(over: Partial<ChatThread> = {}): ChatThread {
  return {
    id: 't1',
    flowId: 'flow1',
    title: '',
    createdAt: '',
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
    ...over,
  }
}

function makeAISettings(): AISettings {
  return {
    activeProvider: 'openai',
    embeddingProvider: 'openai',
    providers: {
      openai: {enabled: true, defaultModel: 'gpt-default', temperature: 0.3, maxTokens: 4096, contextTokenBudget: 8000},
    } as AISettings['providers'],
    saveConversationHistory: true,
    prompts: {} as AISettings['prompts'],
  }
}

const initialChatState = useChatStore.getState()

describe('useChatRequestBuilder', () => {
  beforeEach(() => {
    useChatStore.setState(initialChatState, true)
  })

  it('returns null when there is no document', () => {
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc: null,
        activeThread: makeThread(),
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    expect(result.current.buildRequest('hi')).toBeNull()
  })

  it('returns null when there is no active thread', () => {
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: null,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    expect(result.current.buildRequest('hi')).toBeNull()
  })

  it('builds a request with flowId, provider, model, and contextBlockId', () => {
    const thread = makeThread({contextBlockId: 'b1'})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    const req = result.current.buildRequest('hello')
    expect(req).toMatchObject({
      flowId: 'flow1',
      provider: 'openai',
      model: 'gpt-4',
      userMessage: 'hello',
      contextBlockId: 'b1',
      excludeContext: false,
      useTools: true, // tools are on by default (R-frontend-6)
    })
    expect(req).not.toHaveProperty('messages')
  })

  it('sends an empty model as is (the server default), never the saved default', () => {
    const thread = makeThread()
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: '',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    const req = result.current.buildRequest('hi')
    expect(req?.model).toBe('')
    expect(req?.temperature).toBe(0.3)
    expect(req?.maxTokens).toBe(4096)
  })

  it('includes messages only when includeHistory is true', () => {
    const thread = makeThread()
    useChatStore.setState({threads: [thread]})
    const msgs: ChatMessage[] = [{id: 'm1', role: 'user', content: 'hi', timestamp: 't'}]
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => msgs,
      }),
    )
    expect(result.current.buildRequest('hi')).not.toHaveProperty('messages')
    const withHistory = result.current.buildRequest('hi', undefined, undefined, true)
    expect(withHistory?.messages).toEqual([{id: 'm1', role: 'user', content: 'hi', timestamp: 't'}])
  })

  it('always sends history when "Save conversation history" is off (the server keeps no copy)', () => {
    const thread = makeThread()
    useChatStore.setState({threads: [thread]})
    const msgs: ChatMessage[] = [{id: 'm1', role: 'user', content: 'hi', timestamp: 't'}]
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: {...makeAISettings(), saveConversationHistory: false},
        getMessages: () => msgs,
      }),
    )
    expect(result.current.buildRequest('hi')?.messages).toEqual([
      {id: 'm1', role: 'user', content: 'hi', timestamp: 't'},
    ])
  })

  it("uses the thread's stored selectedSourceFiles when overrideFiles is not given", () => {
    const thread = makeThread({selectedSourceFiles: ['a.txt', 'b.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    expect(result.current.buildRequest('hi')?.selectedSourceFiles).toEqual(['a.txt', 'b.txt'])
  })

  it("overrideFiles takes precedence over the thread's stored selection", () => {
    const thread = makeThread({selectedSourceFiles: ['a.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    expect(result.current.buildRequest('hi', ['c.txt'])?.selectedSourceFiles).toEqual(['c.txt'])
  })

  it('excludeContext clears selectedSourceFiles but keeps contextBlockId', () => {
    const thread = makeThread({contextBlockId: 'b1', selectedSourceFiles: ['a.txt']})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    const req = result.current.buildRequest('hi', undefined, true)
    expect(req?.selectedSourceFiles).toBeUndefined()
    expect(req?.excludeContext).toBe(true)
    expect(req?.contextBlockId).toBe('b1')
  })

  it("reflects the thread's useTools flag", () => {
    const thread = makeThread({useTools: true})
    useChatStore.setState({threads: [thread]})
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: thread,
        provider: 'openai',
        selectedModel: 'gpt-4',
        aiSettings: makeAISettings(),
        getMessages: () => [],
      }),
    )
    expect(result.current.buildRequest('hi')?.useTools).toBe(true)
  })

  // R-frontend-6: tools are on by default (the server falls back to the marker loop for a model without native
  // tools), and a thread's own choice wins over the default.
  describe('useTools default and override (R-frontend-6)', () => {
    function build(thread: ChatThread, ai: AISettings = makeAISettings()) {
      useChatStore.setState({threads: [thread]})
      const {result} = renderHook(() =>
        useChatRequestBuilder({
          doc,
          activeThread: thread,
          provider: 'openai',
          selectedModel: 'gpt-4',
          aiSettings: ai,
          getMessages: () => [],
        }),
      )
      return result.current.buildRequest('hi')
    }

    it('a new thread sends useTools: true', () => {
      const id = useChatStore.getState().createThread('flow1')
      const thread = useChatStore.getState().threads.find(t => t.id === id)!
      expect(build(thread)?.useTools).toBe(true)
    })

    it('a thread whose own useTools is false keeps it', () => {
      expect(build(makeThread({useTools: false}))?.useTools).toBe(false)
    })

    it('the "tools off by default" setting turns an undecided thread off, and leaves a thread that chose alone', () => {
      const ai = {...makeAISettings(), toolsOffByDefault: true}
      expect(build(makeThread(), ai)?.useTools).toBe(false)
      expect(build(makeThread({useTools: true}), ai)?.useTools).toBe(true)
    })

    it('threadUsesTools is the one resolution the header and the request share', () => {
      expect(threadUsesTools(makeThread(), makeAISettings())).toBe(true)
      expect(threadUsesTools(makeThread({useTools: false}), makeAISettings())).toBe(false)
      expect(threadUsesTools(makeThread(), {...makeAISettings(), toolsOffByDefault: true})).toBe(false)
      expect(threadUsesTools(null, makeAISettings())).toBe(false)
    })
  })
})

describe('buildChatRequest history: error bubbles stay UI-only (R-frontend-12)', () => {
  const ERROR_LINE = '*Error: provider timed out*'
  const failed = (id: string, text: string, extra: Partial<ChatMessage> = {}): ChatMessage => ({
    id,
    role: 'assistant',
    content: text ? `${text}\n\n---\n${ERROR_LINE}` : ERROR_LINE,
    timestamp: 't',
    finishReason: 'error',
    ...extra,
  })
  const user = (id: string): ChatMessage => ({id, role: 'user', content: `q ${id}`, timestamp: 't'})
  const answer = (id: string, extra: Partial<ChatMessage> = {}): ChatMessage => ({
    id,
    role: 'assistant',
    content: `a ${id}`,
    timestamp: 't',
    finishReason: 'stop',
    ...extra,
  })

  function build(messages: ChatMessage[], opts: {saveHistory?: boolean; includeHistory?: boolean} = {}) {
    const thread = makeThread()
    return buildChatRequest({
      thread,
      provider: 'openai',
      model: 'gpt-4',
      aiSettings: {...makeAISettings(), saveConversationHistory: opts.saveHistory ?? true},
      messages,
      text: 'next',
      includeHistory: opts.includeHistory ?? false,
    })
  }

  it('leaves an error-finish assistant message out when history saving is off', () => {
    const req = build([user('u1'), failed('e1', '')], {saveHistory: false})
    expect(req.messages?.map(m => m.id)).toEqual(['u1'])
  })

  it('a resend after two failed turns sends no error line: partial text goes without it', () => {
    const req = build([user('u1'), failed('e1', ''), user('u2'), failed('e2', 'partial answer'), user('u3')], {
      includeHistory: true,
    })
    expect(req.messages?.some(m => m.content.includes(ERROR_LINE))).toBe(false)
    expect(req.messages?.some(m => m.content.includes('\n---\n'))).toBe(false)
    expect(req.messages?.map(m => m.id)).toEqual(['u1', 'u2', 'e2', 'u3'])
    expect(req.messages?.find(m => m.id === 'e2')?.content).toBe('partial answer')
  })

  it('drops an empty interrupted message and keeps one that holds a partial answer', () => {
    const req = build(
      [
        user('u1'),
        answer('i1', {content: '', finishReason: 'interrupted'}),
        user('u2'),
        answer('i2', {content: 'half of it', finishReason: 'interrupted'}),
      ],
      {includeHistory: true},
    )
    expect(req.messages?.map(m => m.id)).toEqual(['u1', 'u2', 'i2'])
  })

  it('shares the persistable predicate with the conversation save', () => {
    const messages = [
      user('u1'),
      failed('e1', ''),
      failed('e2', '', {toolCalls: [{name: 'get_block', ok: true}]}),
      failed('e3', 'text before'),
      answer('a1'),
    ]
    const sent = new Set(build(messages, {includeHistory: true}).messages?.map(m => m.id))
    for (const m of messages) {
      if (!isPersistable(m)) expect(sent.has(m.id)).toBe(false)
    }
    // A failed turn the save keeps for its tool calls has no text for the model: nothing of it is sent.
    expect([...sent]).toEqual(['u1', 'e3', 'a1'])
  })
})

describe('useChatRequestBuilder reasoning effort', () => {
  it('sends the configured effort next to temperature/maxTokens and omits the default', () => {
    const settings = makeAISettings()
    const {result} = renderHook(() =>
      useChatRequestBuilder({
        doc,
        activeThread: makeThread(),
        provider: 'openai',
        selectedModel: 'gpt-5',
        aiSettings: settings,
        getMessages: () => [],
      }),
    )
    expectTypeOf<ChatRequest['reasoningEffort']>().toEqualTypeOf<'low' | 'medium' | 'high' | undefined>()
    expectTypeOf<NonNullable<AISettings['providers']['openai']>['reasoningEffort']>().toEqualTypeOf<
      '' | 'low' | 'medium' | 'high' | undefined
    >()
    const plain = result.current.buildRequest('hi')
    expect(plain && 'reasoningEffort' in plain).toBe(false)

    settings.providers.openai = {...settings.providers.openai, reasoningEffort: 'low'}
    const req = result.current.buildRequest('deep question')
    expect(req?.reasoningEffort).toBe('low')
    expect(req?.temperature).toBe(0.3)
    expect(req?.maxTokens).toBe(4096)
  })
})
