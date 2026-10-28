import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import ChatUsageMeter from './ChatUsageMeter'
import {useChatStore, type StreamSlot} from '@/stores/chatStore'
import type {ChatMessage} from '@/types'

function slot(tokens: number): StreamSlot {
  return {
    streamId: 's1',
    messageId: 'm1',
    text: 'partial',
    isThinking: false,
    tokens,
    toolStatus: null,
    toolCalls: [],
    approvals: [],
  }
}

function setTokens(tokens: number) {
  act(() => {
    useChatStore.setState({streams: {t1: slot(tokens)}})
  })
}

/** The element is outside every live region: no role=status/log/alert or aria-live ancestor, or under aria-hidden. */
function silent(el: Element): boolean {
  if (el.closest('[aria-hidden="true"]')) return true
  return !el.closest('[role="status"], [role="log"], [role="alert"], [aria-live]:not([aria-live="off"])')
}

beforeEach(() => {
  useChatStore.setState({activeThreadId: 't1', streams: {}, threads: [], conversations: new Map()})
})
afterEach(() => {
  vi.restoreAllMocks()
})

// R-frontend-4: the meter polled the store on a 250 ms interval and announced every token count through role=status.
describe('ChatUsageMeter', () => {
  it('keeps the status text unchanged across token updates, with the live count outside any live region', () => {
    setTokens(1)
    render(<ChatUsageMeter promptTokens={0} completionTokens={0} />)
    const status = screen.getByRole('status')
    const before = status.textContent
    expect(before).toBe('Generating answer')

    for (const n of [12, 250, 1300, 2048]) {
      setTokens(n)
      expect(screen.getByRole('status').textContent).toBe(before)
      expect(screen.getByRole('status')).not.toHaveAttribute('aria-label')
    }
    const live = screen.getByText('2.0k tokens so far')
    expect(silent(live)).toBe(true)
  })

  it('re-renders on a store update without any timer, and starts no interval', () => {
    const setIntervalSpy = vi.spyOn(globalThis, 'setInterval')
    const windowSetIntervalSpy = vi.spyOn(window, 'setInterval')
    setTokens(3)
    render(<ChatUsageMeter promptTokens={0} completionTokens={0} />)
    expect(screen.getByText('3 tokens so far')).toBeInTheDocument()

    setTokens(42)
    expect(screen.getByText('42 tokens so far')).toBeInTheDocument()
    expect(setIntervalSpy).not.toHaveBeenCalled()
    expect(windowSetIntervalSpy).not.toHaveBeenCalled()
  })

  /** Ends the stream as the engine does: the committed message (if any) lands, then the slot ends. */
  function endWith(message?: Partial<ChatMessage>) {
    act(() => {
      if (message) {
        useChatStore.setState({
          conversations: new Map([
            ['t1', [{id: 'm1', role: 'assistant', content: 'answer', timestamp: 't', ...message} as ChatMessage]],
          ]),
        })
      }
      useChatStore.setState({streams: {}})
    })
  }

  it('announces the done’s output tokens once when the answer finishes', () => {
    setTokens(5)
    const {rerender} = render(<ChatUsageMeter promptTokens={0} completionTokens={0} />)
    setTokens(600)
    endWith({finishReason: 'stop', tokensOut: 640})
    rerender(<ChatUsageMeter promptTokens={100} completionTokens={640} />)
    expect(screen.getByRole('status')).toHaveTextContent('Reply ended, 640 output tokens')

    rerender(<ChatUsageMeter promptTokens={100} completionTokens={640} />)
    expect(screen.getAllByRole('status')).toHaveLength(1)
    expect(screen.getByRole('status')).toHaveTextContent('Reply ended, 640 output tokens')
  })

  it('falls back to the live count when the done carried no token count', () => {
    setTokens(5)
    render(<ChatUsageMeter promptTokens={0} completionTokens={0} />)
    setTokens(77)
    endWith({finishReason: 'stop'})
    expect(screen.getByRole('status')).toHaveTextContent('Reply ended, 77 output tokens')
  })

  it.each([
    ['an error', {finishReason: 'error' as const, tokensOut: 0}],
    ['a Stop with a partial answer', {finishReason: 'interrupted' as const}],
    ['a Stop before any text', undefined],
  ])('announces no token count after %s', (_name, message) => {
    setTokens(5)
    render(<ChatUsageMeter promptTokens={0} completionTokens={0} />)
    setTokens(12)
    endWith(message)
    expect(screen.getByRole('status').textContent).toBe('')
  })

  it('says nothing before any answer streamed', () => {
    render(<ChatUsageMeter promptTokens={10} completionTokens={20} />)
    expect(screen.getByRole('status').textContent).toBe('')
  })
})
