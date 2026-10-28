import {describe, it, expect} from 'vitest'
import {parseChatMessage, parseChatMessages, parseConversationPayload} from './chatMessage'
import type {ChatMessage} from '@/types'

describe('parseChatMessage', () => {
  it('parses a well-formed message', () => {
    const m = parseChatMessage({id: 'm1', role: 'user', content: 'hi', timestamp: '2024-01-01T00:00:00Z'})
    expect(m).toEqual({id: 'm1', role: 'user', content: 'hi', timestamp: '2024-01-01T00:00:00Z'})
  })

  it('preserves valid optional fields', () => {
    const m = parseChatMessage({
      id: 'm1',
      role: 'assistant',
      content: 'hello',
      timestamp: 't',
      contextBlockId: 'b1',
      tokensIn: 10,
      tokensOut: 5,
      model: 'gpt-x',
      provider: 'openai',
      servedModel: 'gpt-x-2024-08-06',
      inputCostPerM: 2.5,
      outputCostPerM: 10,
    })
    expect(m?.contextBlockId).toBe('b1')
    expect(m?.tokensIn).toBe(10)
    expect(m?.model).toBe('gpt-x')
    expect(m?.provider).toBe('openai')
    expect(m?.servedModel).toBe('gpt-x-2024-08-06')
    expect(m && 'inputCostPerM' in m).toBe(false)
    expect(m && 'outputCostPerM' in m).toBe(false)
  })

  it('rejects payloads missing id or timestamp', () => {
    expect(parseChatMessage({role: 'user', content: 'x', timestamp: 't'})).toBeNull()
    expect(parseChatMessage({id: 'm1', role: 'user', content: 'x'})).toBeNull()
  })

  it('rejects non-object payloads', () => {
    expect(parseChatMessage(null)).toBeNull()
    expect(parseChatMessage('hello')).toBeNull()
    expect(parseChatMessage(undefined)).toBeNull()
  })

  it('coerces an unknown role to assistant and defaults non-string content', () => {
    const m = parseChatMessage({id: 'm1', role: 'wizard', content: 42, timestamp: 't'})
    expect(m?.role).toBe('assistant')
    expect(m?.content).toBe('')
  })

  it('drops optional fields of the wrong type', () => {
    const m = parseChatMessage({
      id: 'm1',
      role: 'user',
      content: 'x',
      timestamp: 't',
      tokensIn: 'lots',
      model: 123,
      servedModel: 7,
      inputCostPerM: '2.5',
      modelLabel: 42,
    })
    expect(m).toEqual({id: 'm1', role: 'user', content: 'x', timestamp: 't'})
  })

  it("keeps the model's friendly label and drops an empty one", () => {
    const base = {
      id: 'm1',
      role: 'assistant',
      content: 'a',
      timestamp: 't',
      provider: 'azure-foundry',
      model: 'm/a1b2c3d4e5',
    }
    expect(parseChatMessage({...base, modelLabel: 'Docs writer'})?.modelLabel).toBe('Docs writer')
    expect(parseChatMessage({...base, modelLabel: ''})).not.toHaveProperty('modelLabel')
  })

  it('drops a label that is only an id', () => {
    const base = {id: 'm1', role: 'assistant', content: 'a', timestamp: 't', provider: 'azure-foundry'}
    expect(parseChatMessage({...base, model: 'm/a1b2c3d4e5', modelLabel: 'm/a1b2c3d4e5'})).not.toHaveProperty(
      'modelLabel',
    )
    expect(parseChatMessage({...base, model: 'gpt-4.1', modelLabel: 'm/a1b2c3d4e5'})).not.toHaveProperty('modelLabel')
    expect(parseChatMessage({...base, model: 'gpt-4o', modelLabel: 'gpt-4o'})).not.toHaveProperty('modelLabel')
  })
})

describe('conversation round trip', () => {
  const thread: ChatMessage[] = [
    {id: 'u1', role: 'user', content: 'Document this flow', timestamp: '2026-09-30T10:00:00Z'},
    {
      id: 'a1',
      role: 'assistant',
      content: 'Here is the documentation…',
      timestamp: '2026-09-30T10:00:05Z',
      provider: 'azure-foundry',
      model: 'm/a1b2c3d4e5',
      modelLabel: 'Docs writer',
      servedModel: 'gpt-4.1-2025-04-14',
      tokensIn: 1200,
      tokensOut: 340,
      finishReason: 'stop',
    },
  ]

  const wire = (messages: ChatMessage[]) => JSON.parse(JSON.stringify({messages})).messages as unknown

  it('keeps modelLabel through save → get → save', () => {
    const stored = wire(thread)
    const loaded = parseConversationPayload(stored)
    expect(loaded[1]!.modelLabel).toBe('Docs writer')
    expect(wire(loaded)).toEqual(stored)
    expect(loaded).toEqual(thread)
  })

  it('keeps it through the legacy {messages} load shape too', () => {
    const loaded = parseConversationPayload({messages: wire(thread)})
    expect(loaded[1]).toMatchObject({model: 'm/a1b2c3d4e5', modelLabel: 'Docs writer'})
  })
})

describe('parseChatMessages', () => {
  it('drops invalid entries and keeps valid ones in order', () => {
    const valid: ChatMessage = {id: 'ok', role: 'user', content: 'a', timestamp: 't'}
    const result = parseChatMessages([
      valid,
      {role: 'user', content: 'no id', timestamp: 't'},
      {id: 'ok2', role: 'assistant', content: 'b', timestamp: 't2'},
    ])
    expect(result).toHaveLength(2)
    expect(result[0]!.id).toBe('ok')
    expect(result[1]!.id).toBe('ok2')
  })

  it('returns an empty list for non-array payloads', () => {
    expect(parseChatMessages(undefined)).toEqual([])
    expect(parseChatMessages({messages: []})).toEqual([])
    expect(parseChatMessages(null)).toEqual([])
  })
})

describe('parseConversationPayload', () => {
  const m = {id: 'a', role: 'user', content: 'hi', timestamp: '2024-01-01T00:00:00Z'}

  it('reads the bare array the endpoint returns', () => {
    expect(parseConversationPayload([m])).toHaveLength(1)
  })

  it('still accepts the legacy {messages} file shape', () => {
    expect(parseConversationPayload({messages: [m]})).toHaveLength(1)
  })

  it('treats no body as an empty conversation', () => {
    expect(parseConversationPayload(null)).toEqual([])
    expect(parseConversationPayload(undefined)).toEqual([])
  })

  it('rejects any other shape, so it counts as a failed load and never as "empty"', () => {
    expect(() => parseConversationPayload({version: 1, threads: []})).toThrow()
    expect(() => parseConversationPayload('oops')).toThrow()
  })
})

describe('persisted approvals', () => {
  const fix = {
    proposalId: 'p1',
    ruleId: 'unhandled-error',
    fixType: 'wrap-error-handler',
    blockLabel: 'Call API',
    line: 3,
    summary: 'wrap lines 3-3',
    status: 'applied',
  }
  const approval = {
    proposalId: 'a1',
    tool: 'rename_variable',
    previewKind: 'something-new',
    requiredScope: 'write',
    status: 'applied',
    message: 'renamed',
    items: [{id: 'v1', label: 'Rename x', summary: 'x → total', status: 'applied'}],
  }
  const wire = (messages: ChatMessage[]) => JSON.parse(JSON.stringify({messages})).messages as unknown

  it("keeps a fix proposal's tool and previewKind, and leaves both absent on an older record", () => {
    const m = parseChatMessage({
      id: 'm1',
      role: 'assistant',
      content: '',
      timestamp: 't',
      fixProposals: [
        {...fix, tool: 'apply_fix', previewKind: 'fix-patch'},
        {...fix, proposalId: 'p0'},
      ],
    })
    expect(m?.fixProposals?.[0]).toMatchObject({tool: 'apply_fix', previewKind: 'fix-patch'})
    expect(m?.fixProposals?.[1]).not.toHaveProperty('tool')
    expect(m?.fixProposals?.[1]).not.toHaveProperty('previewKind')
  })

  it('reads message.approvals[] when present and tolerates its absence', () => {
    const m = parseChatMessage({
      id: 'm1',
      role: 'assistant',
      content: '',
      timestamp: 't',
      approvals: [approval, {tool: 'no-id', status: 'applied'}, 'junk', null],
    })
    expect(m?.approvals).toEqual([approval])
    expect(parseChatMessage({id: 'm2', role: 'assistant', content: '', timestamp: 't'})).not.toHaveProperty('approvals')
    expect(
      parseChatMessage({id: 'm3', role: 'assistant', content: '', timestamp: 't', approvals: 'x'}),
    ).not.toHaveProperty('approvals')
  })

  it('keeps both through save → get → save', () => {
    const thread: ChatMessage[] = [
      {
        id: 'a1',
        role: 'assistant',
        content: 'done',
        timestamp: 't',
        fixProposals: [{...fix, tool: 'apply_fix', previewKind: 'fix-patch'}],
        approvals: [approval],
      },
    ]
    const stored = wire(thread)
    const loaded = parseConversationPayload(stored)
    expect(loaded).toEqual(thread)
    expect(wire(loaded)).toEqual(stored)
  })
})

// What a stored turn carries about itself survives a reload: the knowledge notice (R-knowledge-1), the omitted-history
// chip (R-streaming-7) and a failure's code and shared-credential flag.
describe('parseChatMessage turn record fields', () => {
  const base = {id: 'm1', role: 'assistant', content: 'a', timestamp: 't'}

  it('keeps a known knowledge status, the omitted count and the error classification', () => {
    expect(
      parseChatMessage({
        ...base,
        knowledge: 'identity_mismatch',
        truncated: 4,
        finishReason: 'error',
        errorCode: 'CONTEXT_LIMIT',
        errorShared: true,
      }),
    ).toEqual({
      ...base,
      knowledge: 'identity_mismatch',
      truncated: 4,
      finishReason: 'error',
      errorCode: 'CONTEXT_LIMIT',
      errorShared: true,
    })
  })

  it('drops values it cannot trust', () => {
    expect(parseChatMessage({...base, knowledge: 'melted', truncated: -2, errorCode: 7, errorShared: 'yes'})).toEqual(
      base,
    )
    expect(parseChatMessage({...base, truncated: 1.5})).toEqual(base)
  })
})
