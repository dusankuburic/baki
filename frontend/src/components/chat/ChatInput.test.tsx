import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'

// Stub sub-components so we test ChatInput's logic, not their rendering.
vi.mock('./FileAutocomplete', () => ({default: () => null}))
vi.mock('./SlashCommandAutocomplete', () => ({
  default: () => null,
  SLASH_COMMANDS: [],
}))
vi.mock('./ExpandedChatInput', () => ({default: () => null}))
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

import ChatInput from './ChatInput'

beforeEach(() => {
  useChatStore.setState({
    activeThreadId: 'threadA',
    drafts: {},
    stagedPrompt: null,
    streams: {},
    selectedProvider: 'claude',
    conversations: new Map(),
    threads: [
      {
        id: 'threadA',
        flowId: 'f1',
        title: 'T',
        createdAt: '',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    getMessages: (threadId: string) => useChatStore.getState().conversations.get(threadId) ?? [],
  })
  useSettingsStore.setState({settings: {ai: {providers: {}}} as never})
})

describe('ChatInput', () => {
  it('sends on Enter (without shift) and clears the input', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'Hello world')
    fireEvent.keyDown(textarea, {key: 'Enter', shiftKey: false})

    expect(onSend).toHaveBeenCalledWith('Hello world', [], false)
    expect(textarea.value).toBe('')
  })

  it('does NOT send on Shift+Enter', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'Hello')
    fireEvent.keyDown(textarea, {key: 'Enter', shiftKey: true})

    expect(onSend).not.toHaveBeenCalled()
  })

  it('does not send empty input', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    fireEvent.keyDown(textarea, {key: 'Enter', shiftKey: false})

    expect(onSend).not.toHaveBeenCalled()
  })

  it('clears the input and calls onSend on Enter', async () => {
    const onSend = vi.fn()
    useChatStore.setState({drafts: {threadA: 'old draft'}})
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    expect(textarea.value).toBe('old draft')

    fireEvent.change(textarea, {target: {value: 'new message'}})
    await act(async () => {
      fireEvent.keyDown(textarea, {key: 'Enter', shiftKey: false})
    })

    expect(onSend).toHaveBeenCalledWith('new message', [], false)
    // Input is cleared
    expect(textarea.value).toBe('')
  })

  it('seeds from staged prompt when thread matches', () => {
    useChatStore.setState({
      drafts: {threadA: 'old draft'},
      stagedPrompt: {threadId: 'threadA', text: 'Explain this finding'},
    })
    render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    // Staged prompt overrides the draft
    expect(textarea.value).toBe('Explain this finding')
    // Staged prompt is consumed
    expect(useChatStore.getState().stagedPrompt).toBeNull()
  })

  it('does not seed staged prompt for a different thread', () => {
    useChatStore.setState({
      activeThreadId: 'threadA',
      drafts: {threadA: 'my draft'},
      stagedPrompt: {threadId: 'threadB', text: 'should not appear'},
    })
    render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    expect(textarea.value).toBe('my draft')
  })

  it('disables Send when streaming', () => {
    useChatStore.setState({
      streams: {threadA: {streamId: 's1', messageId: 'm1', text: '', isThinking: true, tokens: 0, toolStatus: null, toolCalls: [], fixProposals: []}},
    })
    render(<ChatInput onSend={vi.fn()} />)

    // The send button should reflect disabled state (it uses a Stop icon when streaming)
    const sendBtn = screen.getByRole('button', {name: /stop|cancel/i})
    expect(sendBtn).toBeInTheDocument()
  })

  it('recalls last user message on ArrowUp when input is empty', async () => {
    useChatStore.setState({
      conversations: new Map([
        [
          'threadA',
          [
            {id: 'm1', role: 'user', content: 'first question', timestamp: '', finishReason: 'stop'},
            {id: 'm2', role: 'assistant', content: 'answer', timestamp: '', finishReason: 'stop'},
            {id: 'm3', role: 'user', content: 'second question', timestamp: '', finishReason: 'stop'},
          ],
        ],
      ]),
    })

    render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    expect(textarea.value).toBe('')

    // ArrowUp recalls the most recent user message
    await act(async () => {
      fireEvent.keyDown(textarea, {key: 'ArrowUp'})
    })
    expect(textarea.value).toBe('second question')

    // Set cursor to start so the next ArrowUp fires (the handler requires
    // either empty input or cursor at position 0).
    textarea.setSelectionRange(0, 0)

    // ArrowUp again recalls the previous one
    await act(async () => {
      fireEvent.keyDown(textarea, {key: 'ArrowUp'})
    })
    expect(textarea.value).toBe('first question')

    // ArrowDown goes forward
    await act(async () => {
      fireEvent.keyDown(textarea, {key: 'ArrowDown'})
    })
    expect(textarea.value).toBe('second question')

    // ArrowDown again clears
    await act(async () => {
      fireEvent.keyDown(textarea, {key: 'ArrowDown'})
    })
    expect(textarea.value).toBe('')
  })

  it('does not recall history on ArrowUp when input has text', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'typing something')

    const before = textarea.value
    fireEvent.keyDown(textarea, {key: 'ArrowUp'})
    // Input should not change (ArrowUp only recalls when empty)
    expect(textarea.value).toBe(before)
  })

  it('flushes draft to old thread on thread switch', () => {
    const {rerender} = render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    fireEvent.change(textarea, {target: {value: 'half-typed message'}})

    // Switch active thread
    useChatStore.setState({activeThreadId: 'threadB', drafts: {}})
    rerender(<ChatInput onSend={vi.fn()} />)

    // The old thread's draft should have been saved
    expect(useChatStore.getState().drafts.threadA).toBe('half-typed message')
  })
})

describe('ChatInput queue-while-streaming (U1.6)', () => {
  const streamState = {
    streams: {threadA: {messageId: 'm1', streamId: 's1', text: ''}},
  }

  it('keeps the textarea enabled while streaming', () => {
    useChatStore.setState(streamState as never)
    render(<ChatInput onSend={vi.fn()} />)
    const ta = screen.getByRole('textbox') as HTMLTextAreaElement
    expect(ta.disabled).toBe(false)
  })

  it('Enter during streaming queues the message instead of sending', async () => {
    useChatStore.setState(streamState as never)
    const onSend = vi.fn()
    const onQueue = vi.fn()
    render(<ChatInput onSend={onSend} onQueue={onQueue} />)
    const ta = screen.getByRole('textbox')
    await userEvent.type(ta, 'follow up question')
    fireEvent.keyDown(ta, {key: 'Enter'})
    expect(onQueue).toHaveBeenCalledWith('follow up question', [], false)
    expect(onSend).not.toHaveBeenCalled()
    // The composer cleared — the message lives in the queue now.
    expect((screen.getByRole('textbox') as HTMLTextAreaElement).value).toBe('')
  })

  it('renders the queued chip with the message and lets the user cancel it', () => {
    useChatStore.setState({
      ...streamState,
      queuedByThread: {threadA: {text: 'queued msg', files: []}},
    } as never)
    render(
      <ChatInput
        onSend={vi.fn()}
        onQueue={vi.fn()}
        queued={{text: 'queued msg', files: []}}
        onCancelQueue={() => useChatStore.getState().clearQueuedMessage('threadA')}
      />,
    )
    expect(screen.getByText('queued msg')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Cancel queued message'}))
    expect(useChatStore.getState().queuedByThread['threadA']).toBeUndefined()
  })
})
