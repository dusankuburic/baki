import {describe, it, expect} from 'vitest'
import type {ChatMessage} from '@/types'
import {mergeHistory} from './history'

function msg(id: string, role: ChatMessage['role'] = 'user'): ChatMessage {
  return {id, role, content: id, timestamp: '2024-01-01T00:00:00Z'}
}
const list = (...ids: string[]) => ids.map(id => msg(id))
const ids = (messages: ChatMessage[]) => messages.map(m => m.id)

describe('mergeHistory', () => {
  it('keeps the server order and puts a local-only message after the one it followed', () => {
    expect(ids(mergeHistory(list('a', 'b', 'c'), list('a', 'x', 'b', 'c')))).toEqual(['a', 'x', 'b', 'c'])
    expect(ids(mergeHistory(list('a', 'b', 'c'), list('a', 'b', 'c', 'y')))).toEqual(['a', 'b', 'c', 'y'])
  })

  it('changes nothing when the server holds everything the client has', () => {
    expect(ids(mergeHistory(list('a', 'b', 'c'), list('a', 'b', 'c')))).toEqual(['a', 'b', 'c'])
  })

  it('takes a server-only message where the server has it', () => {
    expect(ids(mergeHistory(list('a', 's', 'b'), list('a', 'b')))).toEqual(['a', 's', 'b'])
  })

  it('puts what the client typed while the history loaded after a history it shares nothing with', () => {
    expect(ids(mergeHistory(list('s1', 's2'), list('typed')))).toEqual(['s1', 's2', 'typed'])
  })

  it('never moves turns the server trimmed from the head to the end', () => {
    // The server keeps the newest 200 messages; the client still shows the older turns e1, e2 above them.
    const merged = mergeHistory(list('l1', 'l2', 'u', 'a'), list('e1', 'e2', 'l1', 'l2', 'u', 'a'))
    expect(ids(merged)).toEqual(['e1', 'e2', 'l1', 'l2', 'u', 'a'])
    // So the part the server holds is in its order and the newest message is still the last one.
    expect(merged.at(-1)!.id).toBe('a')
  })

  it('keeps a trimmed head in front when the turn also added a local message', () => {
    const merged = mergeHistory(list('l1', 'u', 'a'), list('e1', 'l1', 'u', 'x', 'a', 'y'))
    expect(ids(merged)).toEqual(['e1', 'l1', 'u', 'x', 'a', 'y'])
  })
})
