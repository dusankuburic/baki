import {describe, it, expect, vi, beforeEach, onTestFinished} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
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
  useFlowStore.setState({document: null})
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

  it('scopes the list to the active organisation (no flow open) and passes the shared flag through', async () => {
    useOrgStore.setState({activeOrgId: 'org-1'})
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {...PROVIDERS[0], shared: true},
    ] as unknown as ProviderInfo[])
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith('org-1', undefined)
    expect(result.current.currentProvider?.shared).toBe(true)

    // Leaving the org drops the scope and refetches: the org's shared
    // credentials no longer count.
    act(() => useOrgStore.setState({activeOrgId: null}))
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, undefined)
  })

  it("lists the user's own providers unscoped when no organisation is active", async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith(undefined, undefined)
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

// R-2: a chat turn resolves shared credentials through the OPEN flow's org, so
// the picker must list with that org — never the active one while a flow is
// open.
describe('useProviderSetup org scope follows the open flow', () => {
  const AZURE_SHARED = {
    id: 'azure-foundry',
    name: 'Azure AI Foundry',
    configured: true,
    shared: true,
    authType: 'apiKey',
    defaultModel: 'gpt-4.1',
    models: [{id: 'gpt-4.1', displayName: 'gpt-4.1', contextLimit: 128000, inputCostPerM: 0, outputCostPerM: 0}],
  }
  const AZURE_UNCONFIGURED = {...AZURE_SHARED, configured: false, shared: undefined, models: []}
  const openFlow = (orgId?: string) =>
    useFlowStore.setState({
      document: {id: 'f1', name: 'Flow', filePath: '', subflows: [], ...(orgId ? {orgId} : {})},
    } as never)

  it("lists with the open flow's org, not the active one", async () => {
    useOrgStore.setState({activeOrgId: 'org-A'})
    openFlow('org-B')
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith('org-B', undefined)
    expect(providersApi.listProviders).not.toHaveBeenCalledWith('org-A', expect.anything())
  })

  it('lists a personal flow unscoped even under an active org', async () => {
    useOrgStore.setState({activeOrgId: 'org-A'})
    openFlow()
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith(undefined, undefined)
  })

  it('falls back for the session only when a scope change took the provider away', async () => {
    useOrgStore.setState({activeOrgId: 'org-A'})
    openFlow('org-A')
    vi.mocked(providersApi.listProviders).mockImplementation(
      async orgId =>
        (orgId === 'org-A'
          ? [...PROVIDERS, AZURE_SHARED]
          : [...PROVIDERS, AZURE_UNCONFIGURED]) as unknown as ProviderInfo[],
    )
    useChatStore.setState({selectedProvider: 'azure-foundry'})
    const originalUpdateAI = useSettingsStore.getState().updateAI
    const updateAI = vi.fn().mockResolvedValue(undefined)
    useSettingsStore.setState({updateAI})
    onTestFinished(() => useSettingsStore.setState({updateAI: originalUpdateAI}))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.currentProvider?.shared).toBe(true))

    // A personal flow opens: the shared provider is not configured there.
    act(() => openFlow())
    await waitFor(() => expect(useChatStore.getState().selectedProvider).toBe('claude'))
    // The user's saved choice stays for the scopes that do offer it.
    expect(updateAI).not.toHaveBeenCalled()

    // A provider change from Settings in the same scope still writes back.
    act(() => useChatStore.getState().setProvider('azure-foundry'))
    await waitFor(() => expect(updateAI).toHaveBeenCalledWith({activeProvider: 'claude'}))
  })

  it('bypasses the GET micro-cache on an epoch bump', async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, undefined)

    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, {noDedup: true})
  })

  it('ignores a saved Azure default the listing no longer offers', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([
      {...AZURE_SHARED, shared: undefined},
    ] as unknown as ProviderInfo[])
    useChatStore.setState({selectedProvider: 'azure-foundry'})
    const s = useSettingsStore.getState().settings
    useSettingsStore.setState({
      settings: {
        ...s,
        ai: {
          ...s.ai,
          providers: {
            ...s.ai.providers,
            'azure-foundry': {...s.ai.providers['azure-foundry'], defaultModel: 'gone-dep'},
          },
        },
      },
    })
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.selectedModel).toBe('gpt-4.1'))
  })
})
