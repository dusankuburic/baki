import {describe, it, expect, vi, beforeEach, onTestFinished} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useChatStore, threadModelOf, type ChatThread} from '@/stores/chatStore'
import {providerConfig, useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
import type {AIProviderConfig, AISettings, ChatMessage, ModelDetail, ProviderID, ProviderInfo} from '@/types'

vi.mock('@/api', () => ({
  providersApi: {listProviders: vi.fn()},
}))

import {providersApi} from '@/api'
import {useProviderSetup} from './useProviderSetup'

const HAIKU: ModelDetail = {
  id: 'haiku',
  displayName: 'Haiku',
  contextLimit: 200000,
}
const OPUS: ModelDetail = {
  id: 'opus',
  displayName: 'Opus',
  contextLimit: 200000,
  supportsTools: false,
}
const CLAUDE = {
  id: 'claude',
  name: 'Claude',
  configured: true,
  authType: 'apiKey',
  defaultModel: 'haiku',
  models: [HAIKU, OPUS],
}

const GPT: ModelDetail = {
  id: 'm/gpt4100000',
  displayName: 'GPT-4.1',
  contextLimit: 1_000_000,
  kind: 'chat',
  purpose: 'general',
  aliases: ['gpt-4.1'],
  isDefault: true,
}
const DOCS: ModelDetail = {
  id: 'm/docs000000',
  displayName: 'Docs writer',
  contextLimit: 200000,
  kind: 'claude',
  purpose: 'documentation',
  description: 'Writes flow documentation',
  connectionId: 'default',
  connectionName: 'contoso-prod',
  aliases: ['claude:docs'],
}
const azure = (models: ModelDetail[], over: Record<string, unknown> = {}) => ({
  id: 'azure-foundry',
  name: 'Azure AI Foundry',
  configured: true,
  authType: 'api_key',
  defaultModel: GPT.id,
  models,
  ...over,
})
const OPENAI_UNCONFIGURED = {
  id: 'openai',
  name: 'OpenAI',
  configured: false,
  authType: 'apiKey',
  defaultModel: 'gpt-mini',
  models: [],
}

function listing(...providers: object[]) {
  vi.mocked(providersApi.listProviders).mockResolvedValue(providers as unknown as ProviderInfo[])
}

function setAI(activeProvider: ProviderID, defaults: Record<ProviderID, string> = {}) {
  const s = useSettingsStore.getState().settings
  const providers = {...s.ai.providers}
  for (const [id, model] of Object.entries(defaults)) {
    providers[id] = {...providerConfig(s.ai, id), defaultModel: model}
  }
  useSettingsStore.setState({settings: {...s, ai: {...s.ai, activeProvider, providers}}})
}

const T1 = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
const T2 = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'

function seedThread(
  id: string,
  extra: Partial<ChatThread> = {},
  messages: ChatMessage[] = [],
  history: 'loading' | 'loaded' = 'loaded',
) {
  useChatStore.setState(s => ({
    threads: [
      ...s.threads,
      {
        id,
        flowId: 'f1',
        title: '',
        createdAt: '',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
        ...extra,
      },
    ],
    activeThreadId: id,
    conversations: new Map([...s.conversations, [id, messages]]),
    historyStatus: {...s.historyStatus, [id]: {state: history, token: 1}},
  }))
  return id
}

const pinOf = (id: string) => threadModelOf(useChatStore.getState().threads.find(t => t.id === id)!)

let updateProvider: ReturnType<typeof vi.fn<(id: ProviderID, config: Partial<AIProviderConfig>) => Promise<boolean>>>
let updateAI: ReturnType<typeof vi.fn<(ai: Partial<AISettings>) => Promise<void>>>

beforeEach(() => {
  vi.mocked(providersApi.listProviders)
    .mockReset()
    .mockResolvedValue([CLAUDE] as unknown as ProviderInfo[])
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    historyStatus: {},
    modelRefusals: {},
    selectedProvider: 'claude',
    providerEpoch: 0,
  })
  useOrgStore.setState({activeOrgId: null, organisations: []})
  useFlowStore.setState({document: null})
  localStorage.clear()
  const originalUpdateProvider = useSettingsStore.getState().updateProvider
  updateProvider = vi.fn(async (id: ProviderID, config: Partial<AIProviderConfig>): Promise<boolean> => {
    const cur = useSettingsStore.getState().settings
    useSettingsStore.setState({
      settings: {
        ...cur,
        ai: {...cur.ai, providers: {...cur.ai.providers, [id]: {...providerConfig(cur.ai, id), ...config}}},
      },
    })
    return true
  })
  useSettingsStore.setState({updateProvider})
  onTestFinished(() => useSettingsStore.setState({updateProvider: originalUpdateProvider}))
  const originalUpdateAI = useSettingsStore.getState().updateAI
  updateAI = vi.fn(async (_ai: Partial<AISettings>): Promise<void> => {})
  useSettingsStore.setState({updateAI})
  onTestFinished(() => useSettingsStore.setState({updateAI: originalUpdateAI}))
  setAI('claude', {claude: 'haiku', 'azure-foundry': ''})
})

describe('useProviderSetup listing', () => {
  it('passes every listed model field the picker reads through, and nothing else', async () => {
    listing(
      CLAUDE,
      azure([
        {...GPT, secret: 'not for the client'} as ModelDetail,
        {...DOCS, shared: true, status: 'needs_credentials', statusReason: 'Enter the credentials again'},
      ]),
    )
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    const models = result.current.providers.find(p => p.id === 'azure-foundry')!.models
    expect(models[0]).toEqual({
      id: GPT.id,
      displayName: 'GPT-4.1',
      contextLimit: 1_000_000,
      kind: 'chat',
      purpose: 'general',
      aliases: ['gpt-4.1'],
      isDefault: true,
    })
    expect(models[1]).toMatchObject({
      kind: 'claude',
      purpose: 'documentation',
      description: 'Writes flow documentation',
      shared: true,
      connectionId: 'default',
      connectionName: 'contoso-prod',
      status: 'needs_credentials',
      statusReason: 'Enter the credentials again',
      aliases: ['claude:docs'],
    })
    expect(result.current.providers[0]!.models.find(m => m.id === 'opus')?.supportsTools).toBe(false)
  })

  it('lists a provider a newer server added, as sent, and keeps a kind or status it added', async () => {
    const voice = {...GPT, id: 'm/voice00000', displayName: 'Voice', kind: 'realtime', aliases: undefined}
    const paused = {...DOCS, status: 'suspended'}
    listing(
      {...CLAUDE, authType: 'workload_identity'},
      azure([GPT, voice as unknown as ModelDetail, paused as unknown as ModelDetail]),
      {...CLAUDE, id: 'mistral', name: 'Mistral'},
    )
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.providers.map(p => p.id)).toEqual(['claude', 'azure-foundry', 'mistral'])
    expect(result.current.providers[0]!.authType).toBe('workload_identity')
    expect(result.current.providers[2]).toMatchObject({id: 'mistral', name: 'Mistral', configured: true})
    const models = result.current.providers.find(p => p.id === 'azure-foundry')!.models
    expect(models.find(m => m.id === 'm/voice00000')).toMatchObject({kind: 'realtime'})
    expect(models.find(m => m.id === DOCS.id)).toMatchObject({status: 'suspended'})
  })

  it("passes the picker's Review prompt flag through, and only a true one", async () => {
    listing(CLAUDE, azure([GPT], {suggestDiscovery: true}))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.providers.find(p => p.id === 'azure-foundry')?.suggestDiscovery).toBe(true)
    expect(result.current.providers.find(p => p.id === 'claude')).not.toHaveProperty('suggestDiscovery')

    listing(CLAUDE, azure([GPT], {suggestDiscovery: 'yes'}))
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() =>
      expect(result.current.providers.find(p => p.id === 'azure-foundry')).not.toHaveProperty('suggestDiscovery'),
    )
  })

  it('is "checking" (null) until providers load, and reports a failed check', async () => {
    vi.mocked(providersApi.listProviders).mockRejectedValueOnce(new Error('down'))
    const {result} = renderHook(() => useProviderSetup())
    expect(result.current.configured).toBeNull()
    await waitFor(() => expect(result.current.providersError).toBe(true))
    expect(result.current.configured).toBeNull()
    expect(result.current.listingLoaded).toBe(false)
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

  it('scopes the list to the active organisation (no flow open), names it, and passes the shared flag through', async () => {
    useOrgStore.setState({activeOrgId: 'org-1', organisations: [{id: 'org-1', name: 'Contoso'} as never]})
    listing({...CLAUDE, shared: true})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenCalledWith('org-1', undefined)
    expect(result.current.providers[0]!.shared).toBe(true)
    expect(result.current.orgName).toBe('Contoso')

    act(() => {
      useOrgStore.setState({activeOrgId: null})
    })
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, undefined)
  })

  it('bypasses the GET micro-cache on an epoch bump', async () => {
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.providers).toHaveLength(1))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, undefined)

    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, {noDedup: true})
  })
})

describe('useProviderSetup org scope follows the open flow', () => {
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

  it('counts the listing as loaded only once the new scope’s list arrived', async () => {
    let resolveB: (ps: ProviderInfo[]) => void = () => {}
    vi.mocked(providersApi.listProviders).mockImplementation(async orgId =>
      orgId === 'org-B' ? new Promise<ProviderInfo[]>(r => (resolveB = r)) : ([CLAUDE] as unknown as ProviderInfo[]),
    )
    openFlow()
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))

    act(() => openFlow('org-B'))
    await waitFor(() => expect(result.current.listingLoaded).toBe(false))
    await act(async () => resolveB([CLAUDE] as unknown as ProviderInfo[]))
    expect(result.current.listingLoaded).toBe(true)
  })
})

describe('useProviderSetup per-thread model', () => {
  it("starts a new thread on the active provider's Default for new chats, pinning nothing", async () => {
    seedThread(T1)
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.selection).toMatchObject({
      provider: 'claude',
      model: 'haiku',
      label: 'Haiku',
      reason: 'default',
    })
    expect(result.current.turnModel).toEqual({provider: 'claude', model: 'haiku', modelLabel: 'Haiku', pinThread: true})
    expect(pinOf(T1)).toBeNull()
  })

  it('uses the server default when no Default for new chats is saved', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1)
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.selection).toMatchObject({model: GPT.id, label: 'GPT-4.1', reason: 'serverDefault'})
    expect(result.current.threadModelDetail?.id).toBe(GPT.id)
    expect(result.current.newChatDefaults).toEqual({claude: 'haiku', 'azure-foundry': GPT.id})
  })

  it('a header pick pins only its thread and writes no settings', async () => {
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1)
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))

    act(() => result.current.pickModel('azure-foundry', DOCS))
    expect(pinOf(T1)).toEqual({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    expect(result.current.selection).toMatchObject({provider: 'azure-foundry', model: DOCS.id, reason: 'pinned'})
    expect(updateProvider).not.toHaveBeenCalled()
    expect(updateAI).not.toHaveBeenCalled()

    act(() => {
      seedThread(T2)
    })
    expect(result.current.selection).toMatchObject({provider: 'claude', model: 'haiku'})
    act(() => useChatStore.getState().switchThread(T1))
    expect(result.current.selection.model).toBe(DOCS.id)
  })

  it('keeps a pick across settings reloads and listing refetches', async () => {
    seedThread(T1)
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    act(() => result.current.pickModel('claude', OPUS))

    act(() => {
      const s = useSettingsStore.getState().settings
      useSettingsStore.setState({settings: {...s, ai: {...s.ai, providers: {...s.ai.providers}}}})
    })
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(result.current.selection).toMatchObject({model: 'opus', reason: 'pinned'})
  })

  it('a per-thread provider fallback writes no settings', async () => {
    listing(CLAUDE, OPENAI_UNCONFIGURED)
    seedThread(T1, {provider: 'openai', model: 'gpt-mini', modelLabel: 'GPT mini'})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))

    expect(result.current.selection).toMatchObject({provider: 'claude', model: 'haiku', reason: 'fallbackUnavailable'})
    expect(result.current.notice).toMatchObject({kind: 'unavailable', cause: 'provider'})
    expect(result.current.turnModel.pinThread).toBe(false)
    expect(updateAI).not.toHaveBeenCalled()
    expect(updateProvider).not.toHaveBeenCalled()
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('claude')
    expect(useChatStore.getState().selectedProvider).toBe('claude')
    expect(pinOf(T1)?.provider).toBe('openai')
  })

  it("pins a thread from before the upgrade to its last reply's pair, through aliases", async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1, {}, [
      {id: 'u1', role: 'user', content: 'q', timestamp: 't'},
      {id: 'a1', role: 'assistant', content: 'a', timestamp: 't', provider: 'azure-foundry', model: 'claude:docs'},
    ])
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() =>
      expect(pinOf(T1)).toEqual({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'}),
    )
    expect(result.current.selection).toMatchObject({model: DOCS.id, reason: 'pinned'})
    expect(result.current.notice).toBeNull()
  })

  it('a removed model falls back with a notice, and Dismiss pins the fallback', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT]))
    seedThread(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.notice).not.toBeNull())
    expect(result.current.notice).toMatchObject({
      kind: 'removed',
      wanted: {label: 'Docs writer'},
      fallback: {model: GPT.id, label: 'GPT-4.1'},
    })
    expect(result.current.selection.model).toBe(GPT.id)

    act(() => result.current.dismissNotice())
    expect(pinOf(T1)).toEqual({provider: 'azure-foundry', model: GPT.id, modelLabel: 'GPT-4.1'})
    expect(result.current.notice).toBeNull()
  })

  it('raises no notice while the listing loads, or after it failed', async () => {
    seedThread(T1, {provider: 'azure-foundry', model: 'm/gone000000', modelLabel: 'Gone'})
    vi.mocked(providersApi.listProviders).mockReturnValue(new Promise(() => {}))
    const loading = renderHook(() => useProviderSetup())
    expect(loading.result.current.selection).toMatchObject({model: 'm/gone000000', reason: 'pinned'})
    expect(loading.result.current.notice).toBeNull()
    loading.unmount()

    vi.mocked(providersApi.listProviders).mockRejectedValue(new Error('down'))
    const failed = renderHook(() => useProviderSetup())
    await waitFor(() => expect(failed.result.current.providersError).toBe(true))
    expect(failed.result.current.notice).toBeNull()
    expect(failed.result.current.selection.model).toBe('m/gone000000')
  })

  it('keeps the pin while its model is only temporarily unavailable', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, {...DOCS, status: 'needs_credentials'}]))
    seedThread(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.notice).not.toBeNull())
    expect(result.current.notice).toMatchObject({kind: 'unavailable', cause: 'needs_credentials'})
    expect(result.current.turnModel).toMatchObject({model: GPT.id, pinThread: false})
    expect(pinOf(T1)?.model).toBe(DOCS.id)
  })

  it('raises the refusal notice once a listing newer than the refusal still offers the model', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))

    act(() => {
      useChatStore
        .getState()
        .setModelRefusal(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer', epoch: 1})
    })
    expect(result.current.notice).toBeNull()
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(result.current.notice).toMatchObject({kind: 'refused', wanted: {label: 'Docs writer'}}))

    act(() => result.current.dismissNotice())
    expect(useChatStore.getState().modelRefusals).toEqual({})
    expect(result.current.notice).toBeNull()
  })

  it('raises the refusal notice when the refetch after it fails', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))

    vi.mocked(providersApi.listProviders).mockRejectedValue(new Error('network'))
    act(() => {
      useChatStore.getState().bumpProviderEpoch()
      useChatStore
        .getState()
        .setModelRefusal(T1, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer', epoch: 1})
    })
    await waitFor(() => expect(result.current.providersError).toBe(true))
    expect(result.current.notice).toMatchObject({kind: 'refused', wanted: {label: 'Docs writer'}})
  })

  it('does not pin an unpinned thread until its history is in', async () => {
    setAI('azure-foundry')
    listing(CLAUDE, azure([GPT, DOCS]))
    seedThread(T1, {}, [], 'loading')
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.turnModel).toEqual({
      provider: 'azure-foundry',
      model: GPT.id,
      modelLabel: 'GPT-4.1',
      pinThread: false,
    })

    act(() => useChatStore.getState().markHistoryLoaded(T1))
    expect(result.current.turnModel.pinThread).toBe(true)

    act(() => {
      seedThread(T2, {provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'}, [], 'loading')
    })
    expect(result.current.turnModel).toMatchObject({model: DOCS.id, pinThread: true})
  })

  it('never sends an id as the turn’s label', async () => {
    setAI('azure-foundry', {'azure-foundry': DOCS.id})
    vi.mocked(providersApi.listProviders).mockReturnValue(new Promise(() => {}))
    seedThread(T1)
    const {result} = renderHook(() => useProviderSetup())
    expect(result.current.selection).toMatchObject({model: DOCS.id, label: ''})
    expect(result.current.turnModel).toEqual({provider: 'azure-foundry', model: DOCS.id, pinThread: true})
  })
})

describe('the saved Azure default from before the registry', () => {
  it("rewrites one the caller's own entry answers for to its m/ id, once", async () => {
    setAI('azure-foundry', {'azure-foundry': 'gpt-4.1'})
    listing(CLAUDE, azure([GPT, DOCS]))
    const {result, rerender} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(updateProvider).toHaveBeenCalledWith('azure-foundry', {defaultModel: GPT.id}))
    rerender()
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(updateProvider).toHaveBeenCalledTimes(1)
    expect(result.current.legacyDefaultNotice).toBeNull()
  })

  it('does not retry a rewrite whose save failed', async () => {
    setAI('azure-foundry', {'azure-foundry': 'gpt-4.1'})
    listing(CLAUDE, azure([GPT, DOCS]))
    updateProvider.mockResolvedValue(false)
    renderHook(() => useProviderSetup())
    await waitFor(() => expect(updateProvider).toHaveBeenCalledTimes(1))
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenCalledTimes(2))
    expect(updateProvider).toHaveBeenCalledTimes(1)
  })

  it('leaves one only a shared entry answers for', async () => {
    setAI('azure-foundry', {'azure-foundry': 'gpt-4.1'})
    listing(CLAUDE, azure([{...GPT, shared: true}]))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(updateProvider).not.toHaveBeenCalled()
    expect(result.current.legacyDefaultNotice).toBeNull()
  })

  it('says so, once, when it names nothing any more', async () => {
    setAI('azure-foundry', {'azure-foundry': 'gone-deployment'})
    listing(CLAUDE, azure([GPT]))
    const first = renderHook(() => useProviderSetup())
    await waitFor(() =>
      expect(first.result.current.legacyDefaultNotice).toEqual({saved: 'gone-deployment', fallback: 'GPT-4.1'}),
    )
    expect(updateProvider).not.toHaveBeenCalled()

    act(() => first.result.current.dismissLegacyDefaultNotice())
    expect(first.result.current.legacyDefaultNotice).toBeNull()
    first.unmount()

    const second = renderHook(() => useProviderSetup())
    await waitFor(() => expect(second.result.current.listingLoaded).toBe(true))
    expect(second.result.current.legacyDefaultNotice).toBeNull()
  })

  it('says nothing, and rewrites nothing, while Azure lists nothing to chat with', async () => {
    setAI('azure-foundry', {'azure-foundry': 'gpt-4.1'})
    listing(CLAUDE, azure([], {defaultModel: ''}))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.legacyDefaultNotice).toBeNull()
    expect(updateProvider).not.toHaveBeenCalled()
  })

  it('says nothing while new chats do not start on Azure', async () => {
    setAI('claude', {'azure-foundry': 'gone-deployment'})
    listing(CLAUDE, azure([GPT]))
    const {result} = renderHook(() => useProviderSetup())
    await waitFor(() => expect(result.current.listingLoaded).toBe(true))
    expect(result.current.legacyDefaultNotice).toBeNull()
  })
})
