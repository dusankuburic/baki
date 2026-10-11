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
import {deviceFlowSpec, mockProviderCatalog, providerSpec} from '@/testing/fixtures/providerCatalog'
import type {AppInfo, AzureConnection, ProviderInfo, ProviderSpec} from '@/types'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      getProviderCatalog: vi.fn(),
      getGitHubUser: vi.fn(),
      getCopilotUser: vi.fn(),
      getDeviceAuthUser: vi.fn(),
      hasApiKey: vi.fn(),
      deleteApiKey: vi.fn(),
      testProviderConnection: vi.fn(),
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

const notFound = () => new ApiError('not found', 404, null)

function seedCatalog(specs: unknown[]) {
  api.getProviderCatalog.mockResolvedValue(specs as ProviderSpec[])
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
  __resetPersistQueueForTest()
  vi.mocked(settingsApi.updateSettings).mockResolvedValue(undefined)
  useSystemStore.setState({info: null, isLoaded: false})
  api.getProviderCatalog.mockRejectedValue(notFound())
  api.getGitHubUser.mockResolvedValue(null)
  api.getCopilotUser.mockResolvedValue(null)
  api.getDeviceAuthUser.mockResolvedValue(null)
  api.hasApiKey.mockResolvedValue(false)
  api.getLocalModelsConfig.mockResolvedValue({configured: false, baseUrl: ''})
  seedAzure()
})

describe('ProvidersPanel: the legacy panel when the catalogue answers 404', () => {
  it('falls back to the legacy panel: every provider section as a heading, in order, Azure AI Foundry then the local model between GLM and GitHub Models', async () => {
    renderPanel()
    expect(await screen.findByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
    expect(await screen.findByPlaceholderText('http://127.0.0.1:11434/v1')).toBeInTheDocument()

    const headings = SECTION_NAMES.map(name => screen.getByRole('heading', {level: 3, name}))
    for (let i = 1; i < headings.length; i++) {
      const follows = headings[i - 1]!.compareDocumentPosition(headings[i]!) & Node.DOCUMENT_POSITION_FOLLOWING
      expect(follows, `${SECTION_NAMES[i]} follows ${SECTION_NAMES[i - 1]}`).toBeTruthy()
    }
    expect(screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})).toHaveAttribute(
      'id',
      'provider-azure-foundry',
    )
    expect(screen.getByRole('heading', {level: 4, name: 'Connect an Azure AI Foundry resource'})).toBeInTheDocument()
    expect(api.listAzureConnections).toHaveBeenCalledWith({kind: 'user'})
    expect(api.listProviders).not.toHaveBeenCalled()
    expect(api.getProviderCatalog).toHaveBeenCalledTimes(1)
    expect(screen.getByText(/Requires a GitHub account with an active/)).toBeInTheDocument()

    await act(async () => {})
  })

  it('focuses the Azure AI Foundry heading for a deep link once the panel has loaded', async () => {
    useUIStore.setState({settingsAnchor: 'provider-azure-foundry'})
    renderPanel()
    const heading = await screen.findByRole('heading', {level: 3, name: 'Azure AI Foundry'})
    await waitFor(() => expect(heading).toHaveFocus())
    expect(useUIStore.getState().settingsAnchor).toBeNull()
    await act(async () => {})
  })

  it('drops the local model section when the server does not offer local models', async () => {
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

  it('switches through the catalogue-driven panel too', async () => {
    seedCatalog([providerSpec({id: 'claude', name: 'Claude', configured: false}), azureSpec()])
    api.listProviders.mockResolvedValue(providers(false))
    await addFirstChatModel()
    await waitFor(() => expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('azure-foundry'))
  })
})

const azureSpec = () =>
  providerSpec({
    id: 'azure-foundry',
    name: 'Azure AI Foundry',
    credential: {kind: 'document', fields: []},
    endpoints: {test: '/api/providers/test'},
    color: '#0078d4',
  })

describe('ProvidersPanel: catalogue-driven sections', () => {
  it('renders a section per catalogue entry in catalogue order, with provider-<id> anchors', async () => {
    seedCatalog([
      providerSpec({id: 'mistral', name: 'Mistral'}),
      deviceFlowSpec('github-models', {name: 'GitHub Models'}),
      providerSpec({id: 'claude', name: 'Claude', color: '#d4a574'}),
    ])
    renderPanel()
    expect(await screen.findByRole('heading', {level: 3, name: 'Mistral'})).toHaveAttribute('id', 'provider-mistral')
    const names = screen.getAllByRole('heading', {level: 3}).map(h => h.textContent)
    expect(names).toEqual(['Mistral', 'GitHub Models', 'Claude'])
    expect(screen.getByRole('heading', {level: 3, name: 'Claude'})).toHaveAttribute('id', 'provider-claude')
    expect(screen.queryByRole('heading', {level: 3, name: 'xAI (Grok)'})).toBeNull()
    expect(api.listAzureConnections).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.getAllByLabelText('API key')).toHaveLength(2))
    expect(screen.getByRole('button', {name: 'Sign in with GitHub'})).toBeInTheDocument()
    await act(async () => {})
  })

  it('asks for the signed-in user once per device-flow provider, at the catalogue endpoint', async () => {
    seedCatalog([
      deviceFlowSpec('github-models', {name: 'GitHub Models'}),
      deviceFlowSpec('copilot', {name: 'GitHub Copilot'}),
    ])
    api.getDeviceAuthUser.mockImplementation(async path =>
      path === '/api/providers/copilot/user' ? {login: 'octocat', name: 'Octo Cat'} : null,
    )
    renderPanel()
    expect(await screen.findByText('Connected as @octocat')).toBeInTheDocument()
    await act(async () => {})
    expect(api.getDeviceAuthUser.mock.calls.map(c => c[0]).sort()).toEqual([
      '/api/providers/copilot/user',
      '/api/providers/github-models/user',
    ])
    expect(api.getGitHubUser).not.toHaveBeenCalled()
    expect(api.getCopilotUser).not.toHaveBeenCalled()
    expect(screen.queryByText('Or use a Personal Access Token')).toBeNull()
    expect(screen.getByText(/Requires a GitHub account with an active/)).toBeInTheDocument()
  })

  it('renders a provider the bundle does not know from the catalogue as a generic card with Test when configured', async () => {
    seedCatalog(mockProviderCatalog().providers)
    api.testProviderConnection.mockResolvedValue({ok: true, latencyMs: 42})
    renderPanel()
    const heading = await screen.findByRole('heading', {level: 3, name: 'Mistral'})
    expect(heading).toHaveAttribute('id', 'provider-mistral')
    const section = heading.closest('section')!
    expect(within(section).getByText('Configured')).toBeInTheDocument()
    expect(within(section).getByRole('link', {name: 'Set-up guide'})).toHaveAttribute(
      'href',
      'https://docs.example.com/mistral',
    )
    expect(within(section).getByRole('button', {name: 'Remove'})).toBeInTheDocument()
    expect(within(section).queryByText(/newer version of Baki/)).toBeNull()
    fireEvent.click(within(section).getByRole('button', {name: 'Test'}))
    expect(await within(section).findByText('Valid · 42 ms')).toBeInTheDocument()
    expect(api.testProviderConnection).toHaveBeenCalledWith('mistral')
    expect(
      await within(screen.getByRole('heading', {level: 3, name: 'Claude'}).closest('section')!).findByLabelText(
        'API key',
      ),
    ).toBeInTheDocument()
    await act(async () => {})
  })

  it('an unknown credential kind or field type renders the generic card, never an input', async () => {
    seedCatalog([
      providerSpec({id: 'iris', name: 'Iris', credential: {kind: 'biometric', fields: []}}),
      mockProviderCatalog().providers[2]!,
      providerSpec({id: 'orbit', name: 'Orbit', credential: {kind: 'document', fields: []}}),
    ])
    renderPanel()
    expect(await screen.findByRole('heading', {level: 3, name: 'Iris'})).toBeInTheDocument()
    expect(screen.getByRole('heading', {level: 3, name: 'Quantum AI'})).toBeInTheDocument()
    expect(document.querySelectorAll('input, textarea, select')).toHaveLength(0)
    expect(screen.getAllByText("This version of Baki can't show the set-up form for this provider.")).toHaveLength(2)
    expect(screen.getAllByText('This provider is set up by a newer version of Baki.')).toHaveLength(3)
    expect(screen.getAllByText('Not configured')).toHaveLength(3)
    expect(screen.queryByRole('button', {name: 'Test'})).toBeNull()
    await act(async () => {})
  })

  it('shows a retry when the catalogue fails for another reason than 404', async () => {
    api.getProviderCatalog.mockRejectedValueOnce(new ApiError('boom', 500, null))
    seedCatalog([providerSpec({id: 'claude', name: 'Claude'})])
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Retry'}))
    expect(await screen.findByRole('heading', {level: 3, name: 'Claude'})).toBeInTheDocument()
    expect(screen.queryByRole('heading', {level: 3, name: 'Azure AI Foundry'})).toBeNull()
    await act(async () => {})
  })

  it('keeps the local override gated on the backend', async () => {
    seedCatalog([
      providerSpec({id: 'local', name: 'Local', credential: {kind: 'document', fields: []}, endpoints: {}}),
      providerSpec({id: 'claude', name: 'Claude'}),
    ])
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    renderPanel()
    expect(await screen.findByPlaceholderText('http://127.0.0.1:11434/v1')).toBeInTheDocument()
    expect(screen.getByRole('heading', {level: 3, name: 'Local'})).toBeInTheDocument()
    await act(async () => {})
  })

  it('hides a catalogue local entry once the backend is known not to be local', async () => {
    seedCatalog([
      providerSpec({id: 'local', name: 'Local', credential: {kind: 'document', fields: []}, endpoints: {}}),
      providerSpec({id: 'claude', name: 'Claude'}),
    ])
    useSystemStore.setState({info: systemInfo(false), isLoaded: true})
    renderPanel()
    expect(await screen.findByRole('heading', {level: 3, name: 'Claude'})).toBeInTheDocument()
    expect(screen.queryByRole('heading', {level: 3, name: 'Local'})).toBeNull()
    expect(screen.queryByPlaceholderText('http://127.0.0.1:11434/v1')).toBeNull()
    expect(api.getLocalModelsConfig).not.toHaveBeenCalled()
    await act(async () => {})
  })
})
