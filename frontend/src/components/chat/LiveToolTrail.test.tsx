import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, act, fireEvent} from '@testing-library/react'
import LiveToolTrail from './LiveToolTrail'

afterEach(() => {
  vi.useRealTimers()
})
import {useChatStore} from '@/stores/chatStore'

describe('LiveToolTrail', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    useChatStore.setState({
      threads: [],
      streams: {},
      activeThreadId: null,
      conversations: new Map(),
    })
  })

  it('renders nothing when not streaming', () => {
    render(<LiveToolTrail />)
    expect(screen.queryByTestId('live-tool-trail')).not.toBeInTheDocument()
  })

  it('renders nothing while streaming with no tool activity', () => {
    useChatStore.setState({
      activeThreadId: 't1',
      streams: {t1: {streamId: 's1', messageId: 'm1', text: '', isThinking: true, tokens: 0, toolStatus: null, toolCalls: [], fixProposals: []}},
    })
    render(<LiveToolTrail />)
    act(() => {
      vi.advanceTimersByTime(500)
    })
    expect(screen.queryByTestId('live-tool-trail')).not.toBeInTheDocument()
  })

  it('shows the current tool label plus the last finished calls', () => {
    useChatStore.setState({
      activeThreadId: 't1',
      streams: {t1: {
        streamId: 's1',
        messageId: 'm1',
        text: '',
        isThinking: true,
        tokens: 0,
        toolStatus: 'Searching flow',
        toolCalls: [
          {name: 'read_doc', label: 'Reading document', ok: true, durationMs: 900, summary: 'ok'},
          {name: 'list_findings', label: 'Listing findings', ok: true, durationMs: 1100, summary: 'ok'},
          {name: 'search_flow', label: 'Searching flow', ok: true, summary: 'ok'},
          {name: 'get_block', label: 'Reading block', ok: false, durationMs: 50, summary: 'error'},
        ],
        fixProposals: [],
      }},
    })
    render(<LiveToolTrail />)
    act(() => {
      vi.advanceTimersByTime(500)
    })

    expect(screen.getByTestId('live-tool-trail')).toBeInTheDocument()
    expect(screen.getByText('Searching flow…')).toBeInTheDocument()
    // Only the LAST 3 finished calls render (read_doc dropped).
    expect(screen.queryByText('Reading document')).not.toBeInTheDocument()
    expect(screen.getByText('Listing findings')).toBeInTheDocument()
    expect(screen.getByText('Searching flow')).toBeInTheDocument()
    expect(screen.getByText('Reading block')).toBeInTheDocument()
    // Duration rendering + fail dot semantics.
    expect(screen.getByRole('img', {name: 'failed'})).toBeInTheDocument()
    expect(screen.getAllByRole('img', {name: 'succeeded'})).toHaveLength(2)
  })

  it('keeps the finished trail after the status clears (answer streaming)', () => {
    useChatStore.setState({
      activeThreadId: 't1',
      streams: {t1: {
        streamId: 's1', messageId: 'm1', text: 'partial answer', isThinking: false, tokens: 4,
        toolStatus: null,
        toolCalls: [{name: 'search_flow', label: 'Searching flow', ok: true, summary: '2 matches'}],
        fixProposals: [],
      }},
    })
    render(<LiveToolTrail />)
    act(() => {
      vi.advanceTimersByTime(500)
    })
    expect(screen.getByText('Searching flow')).toBeInTheDocument()
    expect(screen.queryByText(/Searching flow…/)).not.toBeInTheDocument()
  })
})

describe('LiveToolTrail expand (V1.3)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  const five = [1, 2, 3, 4, 5].map(i => ({id: `t${i}`, name: `tool${i}`, ok: true, durationMs: 10}))

  it('shows the expand affordance when older calls are hidden, and expands', () => {
    useChatStore.setState({
      activeThreadId: 't1',
      streams: {t1: {streamId: 's1', messageId: 'm1', text: '', isThinking: false, tokens: 0, toolCalls: five, toolStatus: null, fixProposals: []}},
    })
    render(<LiveToolTrail />)
    act(() => {
      vi.advanceTimersByTime(500)
    })
    // 5 total, 3 shown → 2 hidden.
    const expand = screen.getByTestId('tool-trail-expand')
    expect(expand).toHaveTextContent('2 earlier')
    act(() => {
      fireEvent.click(expand)
      vi.advanceTimersByTime(500)
    })
    expect(screen.getByTestId('tool-trail-collapse')).toBeInTheDocument()
    // Expanded shows the earliest call too.
    expect(screen.getByText(/tool1/)).toBeInTheDocument()
    expect(screen.queryByTestId('tool-trail-expand')).toBeNull()
  })
})
