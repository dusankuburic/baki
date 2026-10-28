import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor, act, fireEvent} from '@testing-library/react'
import AITab from './AITab'
import ChatStreamProvider from './ChatStreamProvider'
import {useUIStore} from '@/stores/uiStore'
import {chatApi, providersApi, flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore} from '@/stores/chatStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {ProviderInfo} from '@/types'
import type {ReactNode} from 'react'

vi.mock('react-virtuoso', () => ({
  Virtuoso: ({data = [], itemContent}: {data?: unknown[]; itemContent: (i: number, item: unknown) => ReactNode}) => (
    <div>
      {data.map((item, i) => (
        <div key={i}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}))

vi.mock('@/hooks/useMediaQuery', () => ({useIsDesktop: () => true, useMediaQuery: () => true}))

vi.mock('@/api/client', () => ({
  subscribeToEvents: vi.fn(async () => () => {}),
  subscribeConnectionState: vi.fn((cb: (s: string) => void) => {
    cb('open')
    return () => {}
  }),
  subscribeEventGap: vi.fn(() => () => {}),
  getEventConnectionState: vi.fn(() => 'open'),
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    getSuggestedPrompts: vi.fn(),
    streamChatMessage: vi.fn(),
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    previewContext: vi.fn(),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
  },
  providersApi: {listProviders: vi.fn()},
  flowApi: {getSourceFiles: vi.fn()},
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
  vi.mocked(chatApi.cancelStream).mockResolvedValue(undefined)
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
  useUIStore.setState({chatFocus: false})
})
// R-frontend-6: tools are on by default; a thread switched off stays off, says so in the composer, and a template or
// suggested prompt that needs lookups switches it back on before it sends.
describe('AITab tools default', () => {
  function seedWelcome(useTools?: boolean) {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid-tools')
    vi.mocked(chatApi.getSuggestedPrompts).mockResolvedValue(['Analyze the whole flow'])
    useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
    useChatStore.setState({
      activeThreadId: 't1',
      threads: [
        {
          id: 't1',
          flowId: 'f1',
          title: '',
          createdAt: '2024-01-01T00:00:00Z',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
          ...(useTools === undefined ? {} : {useTools}),
        },
      ],
      conversations: new Map([['t1', []]]),
      historyStatus: {t1: {state: 'loaded', token: 1}},
    })
  }

  const threadTools = () => useChatStore.getState().threads.find(t => t.id === 't1')?.useTools

  it('a template that needs lookups turns tools on before it sends', async () => {
    seedWelcome(false)
    await act(async () => {
      render(<ChatShell />)
    })
    fireEvent.click(await screen.findByRole('button', {name: /Templates/}))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: 'Find bugs'}))
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].useTools).toBe(true)
    expect(threadTools()).toBe(true)
  })

  it('a suggested prompt turns tools on before it sends', async () => {
    seedWelcome(false)
    await act(async () => {
      render(<ChatShell />)
    })
    await act(async () => {
      fireEvent.click(await screen.findByRole('button', {name: 'Analyze the whole flow'}))
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].useTools).toBe(true)
  })

  it('a new thread sends with tools on and shows no tools hint', async () => {
    seedWelcome()
    await act(async () => {
      render(<ChatShell />)
    })
    expect(await screen.findByRole('textbox', {name: /message/i})).toBeInTheDocument()
    expect(screen.queryByText(/Tools are off/)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: /Templates/}))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: 'Explain entire flow'}))
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].useTools).toBe(true)
  })

  it('a tools-off thread with a flow loaded says so in the composer, and the hint turns tools on', async () => {
    seedWelcome(false)
    await act(async () => {
      render(<ChatShell />)
    })
    expect(await screen.findByText(/Tools are off/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Turn on tools'}))
    expect(threadTools()).toBe(true)
    expect(screen.queryByText(/Tools are off/)).not.toBeInTheDocument()
  })
})

// Review of R-frontend-6: a lookup prompt turns tools on only when its send goes ahead, and says so.
describe('AITab lookup prompts on a tools-off thread', () => {
  function seedOff() {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {id: 'claude', name: 'Claude', configured: true, authType: 'apiKey', models: [], defaultModel: 'm1'},
    ] as unknown as ProviderInfo[])
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid-tools')
    useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
    useChatStore.setState({
      activeThreadId: 't1',
      threads: [
        {
          id: 't1',
          flowId: 'f1',
          title: '',
          createdAt: '2024-01-01T00:00:00Z',
          contextBlockId: null,
          selectedSourceFiles: [],
          tokensIn: 0,
          tokensOut: 0,
          useTools: false,
        },
      ],
      conversations: new Map([['t1', []]]),
      historyStatus: {t1: {state: 'loaded', token: 1}},
    })
  }
  const threadTools = () => useChatStore.getState().threads.find(t => t.id === 't1')?.useTools
  const busySlot = (id: string) => ({
    streamId: `s-${id}`,
    messageId: `m-${id}`,
    text: '',
    isThinking: true,
    tokens: 0,
    toolStatus: null,
    toolCalls: [],
    approvals: [],
  })

  it('tells the user tools were turned on for the thread', async () => {
    seedOff()
    await act(async () => {
      render(<ChatShell />)
    })
    fireEvent.click(await screen.findByRole('button', {name: /Templates/}))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: 'Find bugs'}))
    })
    expect(threadTools()).toBe(true)
    expect(await screen.findByText('Tools turned on for this chat')).toBeInTheDocument()
  })

  it('leaves tools off when the send is refused', async () => {
    seedOff()
    useChatStore.setState({streams: {x1: busySlot('x1'), x2: busySlot('x2'), x3: busySlot('x3')} as never})
    await act(async () => {
      render(<ChatShell />)
    })
    fireEvent.click(await screen.findByRole('button', {name: /Templates/}))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: 'Find bugs'}))
    })
    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
    expect(threadTools()).toBe(false)
    expect(screen.queryByText('Tools turned on for this chat')).not.toBeInTheDocument()
  })
})
