import {describe, it, expect, beforeEach} from 'vitest'
import {useChatStore} from '../chatStore'

const initial = useChatStore.getState()

beforeEach(() => {
  useChatStore.setState(initial, true)
})

describe('conversation version in the history status', () => {
  it('a new thread starts at version 0: nothing is stored for it yet', () => {
    const id = useChatStore.getState().createThread('flow1')
    expect(useChatStore.getState().getConversationVersion(id)).toBe(0)
  })

  it('a history load stores the version it was read at; a load without one leaves it unknown', () => {
    const st = useChatStore.getState()
    const id = st.createThread('flow1')
    const token = st.beginHistoryLoad(id)
    expect(useChatStore.getState().getConversationVersion(id)).toBeUndefined()
    useChatStore.getState().finishHistoryLoad(id, token, [], 12)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(12)

    const token2 = useChatStore.getState().beginHistoryLoad(id)
    useChatStore.getState().finishHistoryLoad(id, token2, [])
    expect(useChatStore.getState().getConversationVersion(id)).toBeUndefined()
  })

  it('setConversationVersion keeps the load state and ignores a thread without a status', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().setConversationVersion(id, 5)
    expect(useChatStore.getState().historyStatus[id]).toMatchObject({state: 'loaded', version: 5})
    useChatStore.getState().setConversationVersion('nope', 5)
    expect(useChatStore.getState().historyStatus['nope']).toBeUndefined()
  })

  it('clearing a thread resets its version to 0, the version of a conversation that is gone', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().setConversationVersion(id, 9)
    useChatStore.getState().clearThreadMessages(id)
    expect(useChatStore.getState().getConversationVersion(id)).toBe(0)
  })

  it('closing a thread forgets its version with the rest of its status', () => {
    const id = useChatStore.getState().createThread('flow1')
    useChatStore.getState().setConversationVersion(id, 9)
    useChatStore.getState().closeThread(id)
    expect(useChatStore.getState().getConversationVersion(id)).toBeUndefined()
  })

  it('markHistoryLoaded keeps the version the thread holds', () => {
    const st = useChatStore.getState()
    const id = st.createThread('flow1')
    st.setConversationVersion(id, 9)
    useChatStore.getState().markHistoryLoaded(id)
    expect(useChatStore.getState().historyStatus[id]).toMatchObject({state: 'loaded', version: 9})
  })
})
