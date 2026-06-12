import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, act } from '@testing-library/react'
import AITab from './AITab'
import { chatApi, providersApi, flowApi } from '@/api'
import { useFlowStore } from '@/stores/flowStore'
import { useChatStore } from '@/stores/chatStore'
import type { ConversationFile } from '@/types/domain'
import type { ProviderInfo } from '@/types/domain'

// Prevent real network calls from all API modules.
vi.mock('@/api', () => ({
  chatApi: {
    getSuggestedPrompts: vi.fn(),
    getDemoRemaining: vi.fn(),
    stream: vi.fn(),
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
  vi.mocked(chatApi.getConversation).mockResolvedValue({ messages: [] } as unknown as ConversationFile)
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
})

describe('AITab', () => {
  it('shows setup prompt when no providers are configured', async () => {
    await act(async () => { render(<AITab />) })
    // listProviders resolves [] → configured stays false → shows ApiKeyMissingState
    expect(screen.getByText('Add an API key to start')).toBeInTheDocument()
  })

  it('shows "No flow loaded" state after providers load but no document is open', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {
        id: 'claude', name: 'Claude', configured: true, authType: 'api_key',
        models: [{ id: 'claude-3', displayName: 'Claude 3', contextLimit: 200000, inputCostPerM: 3, outputCostPerM: 15 }],
        defaultModel: 'claude-3',
      },
    ] as ProviderInfo[])

    render(<AITab />)

    // After the listProviders effect resolves, configured becomes true and the flow document is null.
    await waitFor(() => {
      expect(screen.queryByText('Add an API key to start')).not.toBeInTheDocument()
    })
    expect(screen.getByText('No flow loaded')).toBeInTheDocument()
  })

  it('shows "Open settings" button in unconfigured state', async () => {
    await act(async () => { render(<AITab />) })
    expect(screen.getByRole('button', { name: /open settings/i })).toBeInTheDocument()
  })
})
