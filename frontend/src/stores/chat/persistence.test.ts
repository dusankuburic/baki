import {describe, it, expect, beforeEach, afterEach} from 'vitest'
import {persistFlowThreads, readStoredThreads} from './persistence'
import {threadModelOf, type ChatThread} from './types'

const host = window as unknown as Record<string, unknown>
const V1_KEY = 'baki:chatThreads:v1:f1'

function thread(id: string, extra: Partial<ChatThread> = {}): ChatThread {
  return {
    id,
    flowId: 'f1',
    title: '',
    createdAt: '2026-10-10T00:00:00Z',
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
    ...extra,
  }
}

describe('readStoredThreads provider pins', () => {
  beforeEach(() => {
    host.__TAURI_INTERNALS__ = {}
    localStorage.clear()
  })
  afterEach(() => {
    delete host.__TAURI_INTERNALS__
  })

  it('keeps a thread pin whose provider the bundle does not know', () => {
    const turn = {streamId: '11111111-1111-4111-8111-111111111111', provider: 'mistral', model: 'mistral-large'}
    persistFlowThreads('f1', [
      thread('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', {
        provider: 'mistral',
        model: 'mistral-large',
        modelLabel: 'Mistral Large',
        inFlight: turn,
      }),
    ])
    const [restored] = readStoredThreads('f1')
    expect(threadModelOf(restored!)).toEqual({provider: 'mistral', model: 'mistral-large', modelLabel: 'Mistral Large'})
    expect(restored!.inFlight).toEqual(turn)
  })

  it('loads a thread with no provider, or a non-string one, unpinned', () => {
    const base = {flowId: 'f1', title: 't', createdAt: '2026-10-10T00:00:00Z'}
    const turn = {streamId: '11111111-1111-4111-8111-111111111111', model: 'x'}
    localStorage.setItem(
      V1_KEY,
      JSON.stringify([
        {
          ...base,
          id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
          provider: '',
          model: 'x',
          inFlight: {...turn, provider: ''},
        },
        {
          ...base,
          id: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc',
          provider: 7,
          model: 'x',
          inFlight: {...turn, provider: 7},
        },
        {...base, id: 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', provider: 'x'.repeat(65), model: 'x'},
      ]),
    )
    const threads = readStoredThreads('f1')
    expect(threads.map(threadModelOf)).toEqual([null, null, null])
    expect(threads.map(t => t.inFlight)).toEqual([undefined, undefined, undefined])
  })
})
