import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, waitFor, act, fireEvent, within} from '@testing-library/react'
import AITab from './AITab'
import ChatStreamProvider from './ChatStreamProvider'
import ChatPanelShell from './ChatPanelShell'
import {useUIStore} from '@/stores/uiStore'
import {chatApi, providersApi, flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore} from '@/stores/chatStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {ChatMessage, ProviderInfo} from '@/types'
import type {ComponentProps, ReactNode} from 'react'

vi.mock('react-virtuoso', () => ({
  Virtuoso: ({
    data = [],
    itemContent,
    components,
    context,
  }: {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => ReactNode
    components?: {Footer?: (props: {context?: unknown}) => ReactNode}
    context?: unknown
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={i}>{itemContent(i, item)}</div>
      ))}
      {components?.Footer ? <components.Footer context={context} /> : null}
    </div>
  ),
}))

const listRenders = vi.fn()
vi.mock('./ChatMessageList', async importOriginal => {
  const actual = await importOriginal<typeof import('./ChatMessageList')>()
  const {memo} = await import('react')
  const Real = actual.default
  const Counted = memo(function CountedChatMessageList(props: ComponentProps<typeof Real>) {
    listRenders()
    return <Real {...props} />
  })
  return {...actual, default: Counted}
})

vi.mock('@/hooks/useMediaQuery', () => ({useIsDesktop: () => true, useMediaQuery: () => true}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    getSuggestedPrompts: vi.fn(),
    stream: vi.fn(),
    streamChatMessage: vi.fn(),
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
    exportConversation: vi.fn(),
    cancelStream: vi.fn(),
    beginStream: vi.fn(),
    previewContext: vi.fn(),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
  },
  providersApi: {
    listProviders: vi.fn(),
  },
  flowApi: {
    getSourceFiles: vi.fn(),
  },
}))

function ChatShell() {
  return (
    <ToastProvider>
      <ConfirmProvider>
        <ChatStreamProvider>
          <AITab />
        </ChatStreamProvider>
      </ConfirmProvider>
    </ToastProvider>
  )
}

const initialFlowState = useFlowStore.getState()
const initialChatState = useChatStore.getState()

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState(initialFlowState, true)
  useChatStore.setState(initialChatState, true)
  vi.mocked(providersApi.listProviders).mockResolvedValue([])
  vi.mocked(chatApi.getSuggestedPrompts).mockResolvedValue([])
  vi.mocked(chatApi.getConversation).mockResolvedValue({messages: []})
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
  useUIStore.setState({chatFocus: false})
})

describe('AITab', () => {
  it('shows setup prompt when no providers are configured', async () => {
    await act(async () => {
      render(<ChatShell />)
    })
    expect(screen.getByText('Add an API key to start')).toBeInTheDocument()
  })

  it('shows "No flow loaded" state after providers load but no document is open', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {
        id: 'claude',
        name: 'Claude',
        configured: true,
        authType: 'api_key',
        models: [{id: 'claude-3', displayName: 'Claude 3', contextLimit: 200000}],
        defaultModel: 'claude-3',
      },
    ] as ProviderInfo[])

    render(<ChatShell />)

    await waitFor(() => {
      expect(screen.getByText('No flow loaded')).toBeInTheDocument()
    })
    expect(screen.queryByText('Add an API key to start')).not.toBeInTheDocument()
  })

  it('shows "Open settings" button in unconfigured state', async () => {
    await act(async () => {
      render(<ChatShell />)
    })
    expect(screen.getByRole('button', {name: /open settings/i})).toBeInTheDocument()
  })
})

describe('AITab source-file selection (B1)', () => {
  beforeEach(() => {
    ;(window as unknown as Record<string, unknown>).__TAURI_INTERNALS__ = {}
  })
  afterEach(() => {
    delete (window as unknown as Record<string, unknown>).__TAURI_INTERNALS__
  })

  const SOURCE_FILES = [
    {filename: 'Main.txt', subflowId: 'sf1', subflowName: 'Main', blockCount: 3, lineCount: 10},
    {filename: 'Login.txt', subflowId: 'sf2', subflowName: 'Login', blockCount: 2, lineCount: 8},
  ]

  function seedConfiguredChat() {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    vi.mocked(flowApi.getSourceFiles).mockResolvedValue(SOURCE_FILES)
    useFlowStore.setState({
      document: {id: 'f1', name: 'Flow', subflows: [{id: 'sf1', name: 'Main', blocks: []}]} as never,
    })
    useChatStore.setState({
      activeThreadId: 't1',
      threads: [
        {
          id: 't1',
          flowId: 'f1',
          title: 'T',
          createdAt: '2024-01-01T00:00:00Z',
          contextBlockId: null,
          selectedSourceFiles: ['Main.txt', 'Login.txt'],
          tokensIn: 0,
          tokensOut: 0,
        },
      ],
      conversations: new Map([['t1', []]]),
    })
  }

  it('preserves the thread source-file selection across mount', async () => {
    seedConfiguredChat()

    await act(async () => {
      render(<ChatShell />)
    })

    const thread = useChatStore.getState().threads.find(t => t.id === 't1')
    expect(thread?.selectedSourceFiles).toEqual(['Main.txt', 'Login.txt'])
  })

  it('shows the preserved selection in the source-file picker', async () => {
    seedConfiguredChat()

    await act(async () => {
      render(<ChatShell />)
    })

    await waitFor(() => {
      expect(screen.getByText('All 2 files')).toBeInTheDocument()
    })
    expect(screen.queryByText('No files selected')).not.toBeInTheDocument()
  })
})

describe('AITab chat shortcuts', () => {
  function seedChat(streaming: boolean) {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    vi.mocked(chatApi.clearConversation).mockResolvedValue(undefined)
    useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
    useChatStore.setState({
      activeThreadId: 't1',
      threads: [
        {
          id: 't1',
          flowId: 'f1',
          title: 'T',
          createdAt: '2024-01-01T00:00:00Z',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
        },
      ],
      conversations: new Map([
        ['t1', [{id: 'm1', role: 'user' as const, content: 'hi', timestamp: '2024-01-01T00:00:00Z'}]],
      ]),
      streams: streaming
        ? {
            t1: {
              streamId: 'pending',
              messageId: 'm2',
              text: '',
              isThinking: false,
              tokens: 0,
              toolStatus: null,
              toolCalls: [],
            } as never,
          }
        : {},
    })
  }

  async function renderTab() {
    await act(async () => {
      render(<ChatShell />)
    })
    return (await screen.findByRole('textbox', {name: /message/i})) as HTMLTextAreaElement
  }

  it('mod+l clears the thread (after confirming) from the composer, but not from outside the panel', async () => {
    seedChat(false)
    const composer = await renderTab()

    const outside = document.createElement('button')
    document.body.appendChild(outside)
    outside.focus()
    fireEvent.keyDown(outside, {key: 'l', ctrlKey: true})
    expect(chatApi.clearConversation).not.toHaveBeenCalled()
    outside.remove()

    composer.focus()
    fireEvent.keyDown(composer, {key: 'l', ctrlKey: true})
    const dialog = await screen.findByRole('dialog')
    expect(chatApi.clearConversation).not.toHaveBeenCalled()
    fireEvent.click(within(dialog).getByRole('button', {name: 'Clear'}))
    await waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalledTimes(1))
    expect(useChatStore.getState().conversations.get('t1') ?? []).toHaveLength(0)
  })

  it('Escape stops a running reply, and leaves an already-handled Escape alone', async () => {
    seedChat(true)
    const composer = await renderTab()
    composer.focus()

    composer.addEventListener('keydown', e => e.preventDefault(), {once: true})
    fireEvent.keyDown(composer, {key: 'Escape'})
    expect(useChatStore.getState().streams.t1).toBeDefined()

    fireEvent.keyDown(composer, {key: 'Escape'})
    await waitFor(() => expect(useChatStore.getState().streams.t1).toBeUndefined())
  })

  it('Escape declines a pending fix proposal instead of stopping the reply', async () => {
    seedChat(true)
    useChatStore.setState(s => ({
      streams: {
        t1: {
          ...s.streams.t1!,
          approvals: [
            {
              proposalId: 'p1',
              tool: 'apply_fix',
              previewKind: 'fix-patch',
              source: 'fix',
              status: 'pending',
              items: [],
              ruleId: 'r',
              fixType: 'f',
              blockLabel: 'b',
              line: 1,
              summary: 's',
            },
          ],
        },
      },
    }))
    const composer = await renderTab()
    composer.focus()

    fireEvent.keyDown(composer, {key: 'Escape'})

    await waitFor(() => expect(useChatStore.getState().streams.t1?.approvals[0]!.status).toBe('declined'))
    expect(chatApi.respondFixDecision).toHaveBeenCalledWith('pending', 'p1', false, undefined)
    expect(useChatStore.getState().streams.t1).toBeDefined()
    expect(chatApi.cancelStream).not.toHaveBeenCalled()
  })

  it('Escape declines the newest pending card when several are stacked', async () => {
    seedChat(true)
    const card = (proposalId: string) => ({
      proposalId,
      tool: 'apply_fix',
      previewKind: 'fix-patch',
      source: 'fix' as const,
      status: 'pending' as const,
      items: [],
      ruleId: 'r',
      fixType: 'f',
      blockLabel: 'b',
      line: 1,
      summary: 's',
    })
    useChatStore.setState(s => ({streams: {t1: {...s.streams.t1!, approvals: [card('p1'), card('p2')]}}}))
    const composer = await renderTab()
    composer.focus()
    fireEvent.keyDown(composer, {key: 'Escape'})
    await waitFor(() => expect(useChatStore.getState().streams.t1?.approvals[1]!.status).toBe('declined'))
    expect(useChatStore.getState().streams.t1?.approvals[0]!.status).toBe('pending')
    expect(chatApi.respondFixDecision).toHaveBeenCalledWith('pending', 'p2', false, undefined)
  })

  async function renderFocusTab() {
    await act(async () => {
      render(
        <ToastProvider>
          <ConfirmProvider>
            <ChatStreamProvider>
              <ChatPanelShell>
                <AITab />
              </ChatPanelShell>
            </ChatStreamProvider>
          </ConfirmProvider>
        </ToastProvider>,
      )
    })
    return (await screen.findByRole('textbox', {name: /message/i})) as HTMLTextAreaElement
  }

  it('mod+. toggles focus mode from the composer, and Escape leaves it once nothing streams', async () => {
    seedChat(false)
    useUIStore.setState({chatFocus: false, inspectorTab: 'ai', inspectorCollapsed: false})
    const composer = await renderFocusTab()
    composer.focus()
    fireEvent.keyDown(composer, {key: '.', ctrlKey: true})
    expect(useUIStore.getState().chatFocus).toBe(true)
    expect(screen.getByRole('dialog', {name: 'AI Chat'})).toBeInTheDocument()
    fireEvent.keyDown(composer, {key: 'Escape'})
    expect(useUIStore.getState().chatFocus).toBe(false)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('Escape in focus mode stops a running reply first and keeps the overlay', async () => {
    seedChat(true)
    useUIStore.setState({chatFocus: true, inspectorTab: 'ai', inspectorCollapsed: false})
    const composer = await renderFocusTab()
    composer.focus()
    fireEvent.keyDown(composer, {key: 'Escape'})
    await waitFor(() => expect(useChatStore.getState().streams.t1).toBeUndefined())
    expect(useUIStore.getState().chatFocus).toBe(true)
  })

  it('Escape with focus outside the chat leaves focus mode alone', async () => {
    seedChat(false)
    useUIStore.setState({chatFocus: true, inspectorTab: 'ai', inspectorCollapsed: false})
    await renderFocusTab()
    const outside = document.createElement('button')
    document.body.appendChild(outside)
    outside.focus()
    fireEvent.keyDown(outside, {key: 'Escape'})
    expect(useUIStore.getState().chatFocus).toBe(true)
    outside.remove()
  })
})

describe('AITab provider check', () => {
  it('shows neither the setup prompt nor the chat while providers are still being checked', async () => {
    vi.mocked(providersApi.listProviders).mockReturnValue(new Promise(() => {}))
    await act(async () => {
      render(<ChatShell />)
    })
    expect(screen.queryByText('Add an API key to start')).not.toBeInTheDocument()
  })

  it('offers a retry when the provider check fails, instead of claiming no key is set', async () => {
    vi.mocked(providersApi.listProviders).mockRejectedValueOnce(new Error('backend starting'))
    await act(async () => {
      render(<ChatShell />)
    })
    expect(screen.queryByText('Add an API key to start')).not.toBeInTheDocument()
    expect(screen.getByText("Couldn't check your AI providers")).toBeInTheDocument()

    vi.mocked(providersApi.listProviders).mockResolvedValue([])
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    })
    await waitFor(() => expect(screen.getByText('Add an API key to start')).toBeInTheDocument())
  })
})

describe('AITab thread close', () => {
  function seedTwoThreads(firstHasMessages: boolean) {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
    const thread = (id: string, title: string) => ({
      id,
      flowId: 'f1',
      title,
      createdAt: '2024-01-01T00:00:00Z',
      contextBlockId: null,
      selectedSourceFiles: [],
      tokensIn: 0,
      tokensOut: 0,
    })
    useChatStore.setState({
      activeThreadId: 't2',
      threads: [thread('t1', 'Keep me'), thread('t2', 'Other')],
      conversations: new Map([
        ['t1', firstHasMessages ? [{id: 'm1', role: 'user' as const, content: 'hi', timestamp: 'x'}] : []],
        ['t2', []],
      ]),
      historyStatus: {t1: {state: 'loaded', token: 1}, t2: {state: 'loaded', token: 2}},
    })
  }

  it('asks before closing a thread that has messages', async () => {
    seedTwoThreads(true)
    await act(async () => {
      render(<ChatShell />)
    })
    fireEvent.click(await screen.findByRole('button', {name: 'Close Keep me'}))

    const dialog = await screen.findByRole('dialog')
    expect(useChatStore.getState().threads.map(t => t.id)).toContain('t1')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Close thread'}))
    await waitFor(() => expect(useChatStore.getState().threads.map(t => t.id)).not.toContain('t1'))
  })

  it('closes an empty thread straight away', async () => {
    seedTwoThreads(false)
    await act(async () => {
      render(<ChatShell />)
    })
    fireEvent.click(await screen.findByRole('button', {name: 'Close Keep me'}))
    await waitFor(() => expect(useChatStore.getState().threads.map(t => t.id)).not.toContain('t1'))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

describe('AITab retry placement', () => {
  const m = (id: string, role: ChatMessage['role'], content: string, error = false): ChatMessage => ({
    id,
    role,
    content,
    timestamp: '2024-01-01T00:00:00Z',
    ...(error ? {finishReason: 'error' as const} : {}),
  })

  function seed(messages: ChatMessage[]) {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
    useChatStore.setState({
      activeThreadId: 't1',
      threads: [
        {
          id: 't1',
          flowId: 'f1',
          title: 'T',
          createdAt: '2024-01-01T00:00:00Z',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
        },
      ],
      conversations: new Map([['t1', messages]]),
      historyStatus: {t1: {state: 'loaded', token: 1}},
    })
  }

  it('does not offer Retry on an older error once a later reply exists', async () => {
    seed([
      m('u1', 'user', 'first question'),
      m('e1', 'assistant', '*Error: provider exploded*', true),
      m('u2', 'user', 'second question'),
      m('a2', 'assistant', 'a good answer'),
    ])
    await act(async () => {
      render(<ChatShell />)
    })
    await screen.findByText('second question')
    expect(screen.queryByRole('button', {name: 'Retry'})).not.toBeInTheDocument()
  })

  it('offers Retry when the error is the latest reply', async () => {
    seed([m('u1', 'user', 'first question'), m('e1', 'assistant', '*Error: provider exploded*', true)])
    await act(async () => {
      render(<ChatShell />)
    })
    await screen.findByText('first question')
    expect(screen.getAllByRole('button', {name: 'Retry'})).toHaveLength(1)
  })

  it('a canvas selection change re-renders the context bar but not the message list', async () => {
    seed([m('u1', 'user', 'first question'), m('a1', 'assistant', 'an answer'), m('u2', 'user', 'second question')])
    await act(async () => {
      render(<ChatShell />)
    })
    await screen.findByText('second question')
    expect(screen.queryByRole('button', {name: 'Focus selected block'})).not.toBeInTheDocument()
    const before = listRenders.mock.calls.length
    expect(before).toBeGreaterThan(0)

    await act(async () => {
      useFlowStore.getState().selectBlock('b2')
    })
    expect(screen.getByRole('button', {name: 'Focus selected block'})).toBeInTheDocument()
    expect(listRenders).toHaveBeenCalledTimes(before)

    await act(async () => {
      useFlowStore.getState().selectBlock('b3')
    })
    expect(listRenders).toHaveBeenCalledTimes(before)
  })
})
