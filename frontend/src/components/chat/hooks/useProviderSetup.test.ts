import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
import type {ProviderInfo} from '@/types'

vi.mock('@/api', () => ({
  providersApi: {listProviders: vi.fn()},
}))

import {providersApi} from '@/api'
import {useProviderSetup} from './useProviderSetup'

const PROVIDERS = [
  {
    id: 'claude',
    name: 'Claude',
    configured: true,
    authType: 'apiKey',
    defaultModel: 'haiku',
    models: [
      {id: 'haiku', displayName: 'Haiku', contextLimit: 200000, inputCostPerM: 1, outputCostPerM: 5},
      {
        id: 'opus',
        displayName: 'Opus',
        contextLimit: 200000,
        inputCostPerM: 15,
        outputCostPerM: 75,
        supportsTools: false,
      },
    ],
  },
] as unknown as ProviderInfo[]

beforeEach(() => {
  vi.mocked(providersApi.listProviders).mockClear().mockResolvedValue(PROVIDERS)
  useChatStore.setState({selectedProvider: 'claude', providerEpoch: 0})
  useOrgStore.setState({activeOrgId: null})
  // The configured default (Settings → AI) wins over the catalog default.
  const s = useSettingsStore.getState().settings
  useSettingsStore.setState({
    settings: {
      ...s,
      ai: {...s.ai, providers: {...s.ai.providers, claude: {...s.ai.providers.claude, defaultModel: 'haiku'}}},
    },
  })
})

describe('useProviderSetup', () => {
  it('keeps the model the user picked when settings reload', async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.selectedModel).toBe('haiku'))

    act(() => result.current.setSelectedModel('opus'))
    // settings:changed refetches (they also fire for this tab's own layout
    // saves) hand out a new settings.ai.providers object.
    act(() => {
      const s = useSettingsStore.getState().settings
      useSettingsStore.setState({settings: {...s, ai: {...s.ai, providers: {...s.ai.providers}}}})
    })

    expect(result.current.selectedModel).toBe('opus')
  })

  it('starts a newly chosen provider on its default model', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      ...PROVIDERS,
      {
        id: 'openai',
        name: 'OpenAI',
        configured: true,
        authType: 'apiKey',
        defaultModel: 'gpt-mini',
        models: [{id: 'gpt-mini', displayName: 'GPT mini', contextLimit: 128000, inputCostPerM: 1, outputCostPerM: 2}],
      },
    ] as unknown as ProviderInfo[])
    const s = useSettingsStore.getState().settings
    useSettingsStore.setState({
      settings: {
        ...s,
        ai: {...s.ai, providers: {...s.ai.providers, openai: {...s.ai.providers.openai, defaultModel: ''}}},
      },
    })
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.selectedModel).toBe('haiku'))
    act(() => result.current.setSelectedModel('opus'))

    // A different provider: the pick from the old one doesn't apply.
    act(() => result.current.handleSetProvider('openai'))
    await waitFor(() => expect(result.current.selectedModel).toBe('gpt-mini'))
  })

  it('passes the model tool support through for the header hint', async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.currentModels).toHaveLength(2))
    expect(result.current.currentModels.find(m => m.id === 'opus')?.supportsTools).toBe(false)
  })

  it('scopes the list to the active organisation and passes the shared flag through', async () => {
    useOrgStore.setState({activeOrgId: 'org-1'})
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {...PROVIDERS[0], shared: true},
    ] as unknown as ProviderInfo[])
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith('org-1')
    expect(result.current.currentProvider?.shared).toBe(true)

    // Leaving the org drops the scope and refetches: the org's shared
    // credentials no longer count.
    act(() => useOrgStore.setState({activeOrgId: null}))
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined)
  })

  it("lists the user's own providers unscoped when no organisation is active", async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith(undefined)
    expect(result.current.currentProvider?.shared).toBeUndefined()
  })

  it('is "checking" (null) until providers load, and reports a failed check', async () => {
    vi.mocked(providersApi.listProviders).mockRejectedValueOnce(new Error('down'))
    const {result} = renderHook(() => useProviderSetup())
    expect(result.current.configured).toBeNull()
    await waitFor(() => expect(result.current.providersError).toBe(true))
    expect(result.current.configured).toBeNull()
  })
})

describe('useProviderSetup provider changes from outside the header', () => {
  const OPENAI_UNCONFIGURED = {
    id: 'openai',
    name: 'OpenAI',
    configured: false,
    authType: 'apiKey',
    defaultModel: 'gpt-mini',
    models: [],
  }
  const OPENAI = {
    ...OPENAI_UNCONFIGURED,
    configured: true,
    models: [{id: 'gpt-mini', displayName: 'GPT mini', contextLimit: 128000, inputCostPerM: 1, outputCostPerM: 2}],
  }

  it('falls back to a configured provider when the chat is switched to one without a key', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      ...PROVIDERS,
      OPENAI_UNCONFIGURED,
    ] as unknown as ProviderInfo[])
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.selectedModel).toBe('haiku'))

    // Settings → Active Assistant syncs the chat provider directly.
    act(() => useChatStore.getState().setProvider('openai'))

    await waitFor(() => expect(useChatStore.getState().selectedProvider).toBe('claude'))
    // Written back, so Settings shows the provider the chat actually uses.
    await waitFor(() => expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('claude'))
  })

  it("starts on the new provider's default model even when the change came from Settings", async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([...PROVIDERS, OPENAI] as unknown as ProviderInfo[])
    const s = useSettingsStore.getState().settings
    useSettingsStore.setState({
      settings: {
        ...s,
        ai: {...s.ai, providers: {...s.ai.providers, openai: {...s.ai.providers.openai, defaultModel: ''}}},
      },
    })
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.selectedModel).toBe('haiku'))
    act(() => result.current.setSelectedModel('opus'))

    act(() => useChatStore.getState().setProvider('openai'))

    await waitFor(() => expect(result.current.selectedModel).toBe('gpt-mini'))
  })

  it('retrying a failed provider check shows "checking" again, not the stale error', async () => {
    vi.mocked(providersApi.listProviders).mockRejectedValueOnce(new Error('down'))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providersError).toBe(true))

    act(() => result.current.retryProviders())

    expect(result.current.providersError).toBe(false)
    expect(result.current.configured).toBeNull()
    await waitFor(() => expect(result.current.configured).toBe(true))
  })
})
