import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {ToastProvider} from '@/components/shared/Toast'

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
      streams: {
        threadA: {
          streamId: 's1',
          messageId: 'm1',
          text: '',
          isThinking: true,
          tokens: 0,
          toolStatus: null,
          toolCalls: [],
          approvals: [],
        },
      },
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

describe('ChatInput mod+enter', () => {
  it('sends with Ctrl/Cmd+Enter from the composer', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'Hello')
    textarea.focus()
    fireEvent.keyDown(textarea, {key: 'Enter', ctrlKey: true})
    expect(onSend).toHaveBeenCalledWith('Hello', [], false)
  })

  it('ignores the chord outside the composer', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    await userEvent.type(screen.getByRole('textbox'), 'Hello')
    const other = document.createElement('textarea')
    document.body.appendChild(other)
    other.focus()
    const ev = new KeyboardEvent('keydown', {key: 'Enter', ctrlKey: true, bubbles: true, cancelable: true})
    other.dispatchEvent(ev)
    expect(onSend).not.toHaveBeenCalled()
    // Not claimed either: another field's own Ctrl+Enter keeps its default.
    expect(ev.defaultPrevented).toBe(false)
    other.remove()
  })
})

describe("ChatInput keeps the user's text", () => {
  const slot = (streamId: string) => ({
    streamId,
    messageId: `m-${streamId}`,
    text: '',
    isThinking: false,
    tokens: 0,
    toolStatus: null,
    toolCalls: [],
    approvals: [],
  })

  it('does not clear the composer when Enter hits the concurrency cap', async () => {
    // Three OTHER threads are generating: this thread is idle but at the cap.
    useChatStore.setState({streams: {x: slot('s1'), y: slot('s2'), z: slot('s3')}})
    const onSend = vi.fn()
    render(
      <ToastProvider>
        <ChatInput onSend={onSend} />
      </ToastProvider>,
    )

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'keep me')
    fireEvent.keyDown(textarea, {key: 'Enter'})

    // The send would be refused — the message must still be here…
    expect(onSend).not.toHaveBeenCalled()
    expect(textarea.value).toBe('keep me')
    // …and the user told why: the disabled Send button's tooltip is out of
    // reach of an Enter press.
    expect(await screen.findByText('Chats are busy')).toBeInTheDocument()
  })

  it('keeps the stashed draft when the thread switches while a history entry is recalled', async () => {
    useChatStore.setState({
      conversations: new Map([
        ['threadA', [{id: 'u1', role: 'user' as const, content: 'earlier question', timestamp: 'x'}]],
      ]),
    })
    const {rerender} = render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'half-written draft')
    textarea.setSelectionRange(0, 0)
    fireEvent.keyDown(textarea, {key: 'ArrowUp'})
    expect(textarea.value).toBe('earlier question')

    act(() => {
      useChatStore.setState({activeThreadId: 'threadB'})
    })
    rerender(<ChatInput onSend={vi.fn()} />)

    // The draft, not the recalled message, is what the thread keeps.
    expect(useChatStore.getState().drafts.threadA).toBe('half-written draft')
  })

  it('restores the draft after stepping through sent-message history', async () => {
    useChatStore.setState({
      conversations: new Map([
        ['threadA', [{id: 'u1', role: 'user' as const, content: 'earlier question', timestamp: 'x'}]],
      ]),
    })
    render(<ChatInput onSend={vi.fn()} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'half-written draft')
    textarea.setSelectionRange(0, 0)
    fireEvent.keyDown(textarea, {key: 'ArrowUp'})
    expect(textarea.value).toBe('earlier question')

    fireEvent.keyDown(textarea, {key: 'ArrowDown'})
    expect(textarea.value).toBe('half-written draft')
  })

  it('does not send while an IME composition is active', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'nihon')
    // Enter confirms the IME candidate; it must not send the half-composed text.
    fireEvent.keyDown(textarea, {key: 'Enter', isComposing: true})

    expect(onSend).not.toHaveBeenCalled()
  })

  it('does not send on the Enter WebKit delivers after the composition ended (keyCode 229)', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'nihon')
    // Safari / WKWebView: compositionend has fired, isComposing is already
    // false, and the candidate-confirming Enter carries the 229 keyCode.
    fireEvent.keyDown(textarea, {key: 'Enter', keyCode: 229, isComposing: false})

    expect(onSend).not.toHaveBeenCalled()
    expect(textarea.value).toBe('nihon')
  })

  it('clears the composer only once a previewed message is actually sent', async () => {
    const onPreview = vi.fn()
    render(<ChatInput onSend={vi.fn()} onPreview={onPreview} />)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'check the context first')
    fireEvent.click(screen.getByRole('button', {name: /preview context/i}))

    expect(onPreview).toHaveBeenCalledWith('check the context first', [], false, expect.any(Function))
    // Preview open (or cancelled): the draft stays.
    expect(textarea.value).toBe('check the context first')

    // Confirmed and sent: now it clears.
    const onConfirmed = onPreview.mock.calls[0]![3] as () => void
    act(() => onConfirmed())
    expect(textarea.value).toBe('')
  })
})

// A frame for the caret restore that insertTrigger schedules.
async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

describe('ChatInput toolbar', () => {
  it('inserts @ at the caret with a separating space', async () => {
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'hello')
    fireEvent.click(screen.getByRole('button', {name: 'Files'}))
    expect(textarea.value).toBe('hello @')
  })

  it('inserts / into an empty composer without a leading space', () => {
    render(<ChatInput onSend={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', {name: 'Commands'}))
    expect((screen.getByRole('textbox') as HTMLTextAreaElement).value).toBe('/')
  })

  it('inserts mid-text and leaves the caret right after the trigger', async () => {
    render(<ChatInput onSend={vi.fn()} />)
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'hello world')
    textarea.setSelectionRange(5, 5)
    fireEvent.click(screen.getByRole('button', {name: 'Files'}))
    expect(textarea.value).toBe('hello @ world')
    await flushFrame()
    expect(textarea.selectionStart).toBe(7)
    expect(document.activeElement).toBe(textarea)
  })

  it('sends with the context excluded once the toggle is on', async () => {
    const onSend = vi.fn()
    render(<ChatInput onSend={onSend} />)
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'no context please')
    const toggle = screen.getByRole('checkbox', {name: 'Exclude context'})
    fireEvent.click(toggle)
    expect(toggle).toBeChecked()
    fireEvent.keyDown(textarea, {key: 'Enter'})
    expect(onSend).toHaveBeenCalledWith('no context please', [], true)
  })

  it('greys out Send until there is something to send', async () => {
    render(<ChatInput onSend={vi.fn()} />)
    const send = screen.getByRole('button', {name: 'Send message'})
    expect(send).toBeDisabled()
    await userEvent.type(screen.getByRole('textbox'), 'x')
    expect(send).toBeEnabled()
  })
})

// Azure AI Foundry with nothing to chat with (no model added, or every
// resource needs its credentials again): the composer says so instead of
// sending a request with no model.
describe('ChatInput guidance', () => {
  it('shows the guidance with its action, and blocks sending — not typing', async () => {
    const onSend = vi.fn()
    const onAction = vi.fn()
    render(
      <ChatInput
        onSend={onSend}
        onPreview={vi.fn()}
        guidance={{text: 'No Azure AI Foundry models yet.', actionLabel: 'Add models…', onAction}}
      />,
    )
    expect(screen.getByText('No Azure AI Foundry models yet.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Add models…'}))
    expect(onAction).toHaveBeenCalledTimes(1)

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    await userEvent.type(textarea, 'hello')
    expect(textarea.value).toBe('hello')
    fireEvent.keyDown(textarea, {key: 'Enter'})
    expect(onSend).not.toHaveBeenCalled()
    expect(textarea.value).toBe('hello')
    expect(screen.getByRole('button', {name: 'Send message'})).toBeDisabled()
    expect(screen.queryByRole('button', {name: 'Preview context'})).toBeNull()
  })
})
