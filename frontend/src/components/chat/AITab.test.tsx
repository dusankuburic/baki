import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor, act} from '@testing-library/react'
import AITab from './AITab'
import {chatApi, providersApi, flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore} from '@/stores/chatStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {ConversationFile} from '@/types'
import type {ProviderInfo} from '@/types'

// Prevent real network calls from all API modules.
vi.mock('@/api', () => ({
  chatApi: {
    getSuggestedPrompts: vi.fn(),
    getDemoRemaining: vi.fn(),
    stream: vi.fn(),
    streamChatMessage: vi.fn(),
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
    exportConversation: vi.fn(),
    cancelStream: vi.fn(),
    beginStream: vi.fn(),
    previewContext: vi.fn(),
  },
  providersApi: {
    listProviders: vi.fn(),
  },
  flowApi: {
    getSourceFiles: vi.fn(),
  },
}))

// Capture initial store states for clean resets.
const initialFlowState = useFlowStore.getState()
const initialChatState = useChatStore.getState()

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState(initialFlowState, true)
  useChatStore.setState(initialChatState, true)
  // Re-establish default return values; clearAllMocks preserves implementations
  // so test-specific overrides from a previous test would otherwise leak.
  vi.mocked(providersApi.listProviders).mockResolvedValue([])
  vi.mocked(chatApi.getSuggestedPrompts).mockResolvedValue([])
  vi.mocked(chatApi.getDemoRemaining).mockResolvedValue(5)
  vi.mocked(chatApi.getConversation).mockResolvedValue({messages: []} as unknown as ConversationFile)
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
})

describe('AITab', () => {
  it('shows setup prompt when no providers are configured', async () => {
    await act(async () => {
      render(
        <ToastProvider>
          <ConfirmProvider>
            <AITab />
          </ConfirmProvider>
        </ToastProvider>,
      )
    })
    // listProviders resolves [] → configured stays false → shows ApiKeyMissingState
    expect(screen.getByText('Add an API key to start')).toBeInTheDocument()
  })

  it('shows "No flow loaded" state after providers load but no document is open', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {
        id: 'claude',
        name: 'Claude',
        configured: true,
        authType: 'api_key',
        models: [{id: 'claude-3', displayName: 'Claude 3', contextLimit: 200000, inputCostPerM: 3, outputCostPerM: 15}],
        defaultModel: 'claude-3',
      },
    ] as ProviderInfo[])

    render(
      <ToastProvider>
        <ConfirmProvider>
          <AITab />
        </ConfirmProvider>
      </ToastProvider>,
    )

    // After the listProviders effect resolves, configured becomes true and the flow document is null.
    await waitFor(() => {
      expect(screen.queryByText('Add an API key to start')).not.toBeInTheDocument()
    })
    expect(screen.getByText('No flow loaded')).toBeInTheDocument()
  })

  it('shows "Open settings" button in unconfigured state', async () => {
    await act(async () => {
      render(
        <ToastProvider>
          <ConfirmProvider>
            <AITab />
          </ConfirmProvider>
        </ToastProvider>,
      )
    })
    expect(screen.getByRole('button', {name: /open settings/i})).toBeInTheDocument()
  })
})

// Regression: the AI tab must not clobber the thread's source-file selection.
// ChatInput used to mirror its LOCAL @-mention array (`taggedFiles`) into the
// thread via an `onFilesChange` effect. That array starts [] and is reset to []
// after every send, so the effect wrote an empty selection on mount, after each
// message, and again whenever the active thread changed (which changes the
// callback's identity and re-fires the effect). The result: the AI silently lost
// all source-file context from the second message onward, and the picker read
// "No files selected" though the user never deselected anything.
//
// Per-message @-mention overrides already travel correctly as onSend's `files`
// argument (→ buildRequest's `overrideFiles`); the thread's persistent selection
// belongs to SourceFilePicker alone. These two concepts must not share a writer.
describe('AITab source-file selection (B1)', () => {
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
      render(
        <ToastProvider>
          <ConfirmProvider>
            <AITab />
          </ConfirmProvider>
        </ToastProvider>,
      )
    })

    const thread = useChatStore.getState().threads.find(t => t.id === 't1')
    expect(thread?.selectedSourceFiles).toEqual(['Main.txt', 'Login.txt'])
  })

  it('shows the preserved selection in the source-file picker', async () => {
    seedConfiguredChat()

    await act(async () => {
      render(
        <ToastProvider>
          <ConfirmProvider>
            <AITab />
          </ConfirmProvider>
        </ToastProvider>,
      )
    })

    await waitFor(() => {
      expect(screen.getByText('All 2 files')).toBeInTheDocument()
    })
    expect(screen.queryByText('No files selected')).not.toBeInTheDocument()
  })
})
