import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, act, waitFor, fireEvent, within} from '@testing-library/react'
import ProvidersPanel from './ProvidersPanel'
import {ConfirmProvider} from '@/components/shared'
import {providersApi, settingsApi} from '@/api'
import {ApiError} from '@/api/client'
import {resetAllStores} from '@/stores/storeRegistry'
import {useSystemStore} from '@/stores/systemStore'
import {useSettingsStore, __resetPersistQueueForTest} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import type {AppInfo, AzureConnection, ProviderInfo} from '@/types'

// Every section does a lookup on mount: the key inputs ask hasApiKey, the
// device-flow buttons ask for the signed-in user, and the Azure AI Foundry
// panel loads its resources and models. ConfirmProvider is required by the
// Azure panel's remove/discard confirms.
vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      getGitHubUser: vi.fn(),
      getCopilotUser: vi.fn(),
      hasApiKey: vi.fn(),
      getLocalModelsConfig: vi.fn(),
      listProviders: vi.fn(),
      listAzureConnections: vi.fn(),
      listAzureModels: vi.fn(),
      createAzureModel: vi.fn(),
    },
    settingsApi: {...actual.settingsApi, updateSettings: vi.fn()},
  }
})

const api = vi.mocked(providersApi)

const systemInfo = (sessionAnalytics: boolean): AppInfo => ({
  version: 'dev',
  platform: 'linux',
  arch: 'amd64',
  buildDate: '',
  gitCommit: '',
  capabilities: {sessionAnalytics},
})

const SECTION_NAMES = [
  'Claude',
  'OpenAI',
  'Gemini',
  'xAI (Grok)',
  'GLM (z.ai)',
  'Azure AI Foundry',
  'Local model (OpenAI-compatible)',
  'GitHub Models',
  'GitHub Copilot',
]

const resource: AzureConnection = {
  id: 'default',
  name: 'my-res',
  shared: false,
  legacy: true,
  config: {endpoint: 'https://my-res.openai.azure.com', authMode: 'api_key', hasSecret: true},
  status: 'ready',
  canDiscover: false,
  supportsAgents: false,
  suggestDiscovery: false,
  modelCount: 0,
  updatedAt: '2026-09-30T10:00:00Z',
}

function seedAzure(connections: AzureConnection[] = []) {
  api.listAzureConnections.mockResolvedValue({
    connections,
    registryState: 'ok',
    limits: {connections: 10, models: 50},
    knowledgeEmbeddingEnabled: false,
  })
  api.listAzureModels.mockResolvedValue({models: []})
}

function renderPanel() {
  return render(
    <ConfirmProvider>
      <ProvidersPanel />
    </ConfirmProvider>,
  )
}

beforeEach(async () => {
  vi.resetAllMocks()
  await resetAllStores()
  // Switching the active provider saves the settings (debounced): a pending
  // save of the previous case must not fire into this one.
  __resetPersistQueueForTest()
  vi.mocked(settingsApi.updateSettings).mockResolvedValue(undefined)
  // Not loaded yet: the local section asks the server itself.
  useSystemStore.setState({info: null, isLoaded: false})
  api.getGitHubUser.mockResolvedValue(null)
  api.getCopilotUser.mockResolvedValue(null)
  api.hasApiKey.mockResolvedValue(false)
  api.getLocalModelsConfig.mockResolvedValue({configured: false, baseUrl: ''})
  seedAzure()
})

describe('ProvidersPanel', () => {
  it('renders every provider section as a heading, in order, Azure AI Foundry then the local model between GLM and GitHub Models', async () => {
    renderPanel()
    // The Azure and local blocks appear once their own GETs resolve.
    expect(await screen.findByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
    expect(await screen.findByPlaceholderText('http://127.0.0.1:11434/v1')).toBeInTheDocument()

    const headings = SECTION_NAMES.map(name => screen.getByRole('heading', {level: 3, name}))
    for (let i = 1; i < headings.length; i++) {
      const follows = headings[i - 1]!.compareDocumentPosition(headings[i]!) & Node.DOCUMENT_POSITION_FOLLOWING
      expect(follows, `${SECTION_NAMES[i]} follows ${SECTION_NAMES[i - 1]}`).toBeTruthy()
    }
    // The Azure heading is the "Manage models…" deep link's anchor.
    expect(screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})).toHaveAttribute(
      'id',
      'provider-azure-foundry',
    )
    // The resource cards sit one level below.
    expect(screen.getByRole('heading', {level: 4, name: 'Connect an Azure AI Foundry resource'})).toBeInTheDocument()
    expect(api.listAzureConnections).toHaveBeenCalledWith({kind: 'user'})
    // The provider list itself is not fetched on mount: nothing reads it.
    expect(api.listProviders).not.toHaveBeenCalled()
    expect(screen.getByText(/Requires a GitHub account with an active/)).toBeInTheDocument()

    // Let the remaining mount-time lookups settle inside act.
    await act(async () => {})
  })

  it('focuses the Azure AI Foundry heading for a deep link once the panel has loaded', async () => {
    useUIStore.setState({settingsAnchor: 'provider-azure-foundry'})
    renderPanel()
    const heading = screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})
    await waitFor(() => expect(heading).toHaveFocus())
    expect(useUIStore.getState().settingsAnchor).toBeNull()
    await act(async () => {})
  })

  it('drops the local model section when the server does not offer local models', async () => {
    // What a cloud server's GET /api/providers/local/config answers.
    api.getLocalModelsConfig.mockRejectedValue(
      new ApiError('local models are available in the desktop app only', 400, 'LOCAL_MODELS_UNAVAILABLE'),
    )
    renderPanel()
    expect(await screen.findByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('Local model (OpenAI-compatible)')).not.toBeInTheDocument())
    expect(screen.getByText('GitHub Models')).toBeInTheDocument()
  })

  it('does not render the local model section at all once the backend is known not to be local', async () => {
    useSystemStore.setState({info: systemInfo(false), isLoaded: true})
    renderPanel()
    expect(await screen.findByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
    expect(screen.queryByText('Local model (OpenAI-compatible)')).not.toBeInTheDocument()
    expect(api.getLocalModelsConfig).not.toHaveBeenCalled()
    await act(async () => {})
  })

  it('renders the local model section on a local backend', async () => {
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    renderPanel()
    expect(await screen.findByPlaceholderText('http://127.0.0.1:11434/v1')).toBeInTheDocument()
    expect(screen.getByText('Local model (OpenAI-compatible)')).toBeInTheDocument()
    await act(async () => {})
  })

  it('translates the Disconnect label for a signed-in GitHub user', async () => {
    api.getGitHubUser.mockResolvedValue({login: 'octocat', name: 'Octo Cat'})
    renderPanel()
    expect(await screen.findByRole('button', {name: 'Disconnect'})).toBeInTheDocument()
    expect(screen.getByText(/Connected as/)).toBeInTheDocument()

    await act(async () => {})
  })
})

// Like the Copilot and GitHub sign-ins, the first Azure chat model makes
// Azure the active provider — but only when the current one cannot chat.
describe('ProvidersPanel: the first Azure AI Foundry chat model', () => {
  const providers = (claudeConfigured: boolean): ProviderInfo[] => [
    {
      id: 'claude',
      name: 'Claude',
      authType: 'api_key',
      contextLimit: 0,
      models: [],
      defaultModel: '',
      configured: claudeConfigured,
    },
  ]

  async function addFirstChatModel() {
    seedAzure([resource])
    api.createAzureModel.mockResolvedValue({
      id: 'gpt4100000',
      modelId: 'm/gpt4100000',
      connectionId: 'default',
      connectionName: 'my-res',
      kind: 'chat',
      deployment: 'gpt-4.1',
      target: 'gpt-4.1',
      displayName: 'gpt-4.1',
      purpose: 'general',
      isDefault: false,
      isKnowledgeEmbedding: false,
      shared: false,
      status: 'ready',
      updatedAt: '2026-09-30T10:00:00Z',
    })
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Add model to my-res'}))
    const dialog = screen.getByRole('dialog', {name: 'Add model'})
    fireEvent.change(within(dialog).getByRole('textbox', {name: 'Deployment name'}), {target: {value: 'gpt-4.1'}})
    fireEvent.click(within(dialog).getByRole('button', {name: 'Add model'}))
    await waitFor(() => expect(api.createAzureModel).toHaveBeenCalledTimes(1))
  }

  it('switches to Azure AI Foundry when the active provider is not configured', async () => {
    api.listProviders.mockResolvedValue(providers(false))
    await addFirstChatModel()
    // New chats take their provider from the setting.
    await waitFor(() => expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('azure-foundry'))
    expect(api.listProviders).toHaveBeenCalledWith(undefined, {noDedup: true})
  })

  it('keeps a configured active provider', async () => {
    api.listProviders.mockResolvedValue(providers(true))
    await addFirstChatModel()
    await waitFor(() => expect(api.listProviders).toHaveBeenCalledTimes(1))
    await act(async () => {})
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('claude')
  })

  it('leaves the provider alone when the check fails', async () => {
    api.listProviders.mockRejectedValue(new Error('offline'))
    await addFirstChatModel()
    await waitFor(() => expect(api.listProviders).toHaveBeenCalledTimes(1))
    await act(async () => {})
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('claude')
  })
})
