import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import AzureFoundryPanel, {type AzureFoundryPanelProps} from './AzureFoundryPanel'
import {providersApi, settingsApi} from '@/api'
import {ApiError, VersionConflictError} from '@/api/client'
import {resetAllStores} from '@/stores/storeRegistry'
import {useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {useSettingsStore, __resetPersistQueueForTest} from '@/stores/settingsStore'
import {useSystemStore} from '@/stores/systemStore'
import {useUIStore} from '@/stores/uiStore'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {unknownValue} from '@/testing/testHelpers'
import type {AppInfo, AzureConnection, AzureConnectionsListing, AzureModelEntry, AzureScope} from '@/types'

const confirmMock = vi.hoisted(() => vi.fn())
vi.mock('@/components/shared', async importOriginal => ({
  ...(await importOriginal<typeof import('@/components/shared')>()),
  useConfirm: () => ({confirm: confirmMock, prompt: vi.fn()}),
}))

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      ...actual.providersApi,
      listAzureConnections: vi.fn(),
      listAzureModels: vi.fn(),
      createAzureConnection: vi.fn(),
      updateAzureConnection: vi.fn(),
      deleteAzureConnection: vi.fn(),
      discoverAzureConnection: vi.fn(),
      createAzureModel: vi.fn(),
      updateAzureModel: vi.fn(),
      deleteAzureModel: vi.fn(),
      testAzure: vi.fn(),
    },
    settingsApi: {...actual.settingsApi, updateSettings: vi.fn()},
  }
})

const api = vi.mocked(providersApi)
const USER: AzureScope = {kind: 'user'}
const ORG: AzureScope = {kind: 'org', orgId: 'org-1'}
const ANCHOR = 'provider-azure-foundry'
const initialSettings = useSettingsStore.getState().settings
const realUpdateProvider = useSettingsStore.getState().updateProvider

function conn(id: string, over: Partial<AzureConnection> = {}): AzureConnection {
  return {
    id,
    name: id === 'default' ? 'my-res' : id,
    shared: false,
    legacy: id === 'default',
    config: {
      endpoint: `https://${id === 'default' ? 'my-res' : id}.openai.azure.com`,
      authMode: 'api_key',
      hasSecret: true,
    },
    status: 'ready',
    canDiscover: false,
    supportsAgents: false,
    suggestDiscovery: false,
    modelCount: 0,
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

const entraConn = (id: string, over: Partial<AzureConnection> = {}) =>
  conn(id, {
    config: {
      endpoint: `https://${id}.services.ai.azure.com`,
      authMode: 'entra',
      hasSecret: true,
      tenantId: '11111111-2222-4333-8444-555555555555',
      clientId: 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee',
      projectEndpoint: `https://${id}.services.ai.azure.com/api/projects/p`,
    },
    canDiscover: true,
    supportsAgents: true,
    ...over,
  })

function model(id: string, over: Partial<AzureModelEntry> = {}): AzureModelEntry {
  const deployment = over.deployment ?? id
  return {
    id,
    modelId: `m/${id}`,
    connectionId: 'default',
    connectionName: 'my-res',
    kind: 'chat',
    deployment,
    target: deployment,
    displayName: deployment,
    purpose: 'general',
    isDefault: false,
    isKnowledgeEmbedding: false,
    shared: false,
    status: 'ready',
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

const gpt = model('gpt4100000', {deployment: 'gpt-4.1', displayName: 'GPT-4.1', contextWindow: 1_000_000})
const docs = model('docs000000', {
  deployment: 'claude-sonnet-4-6',
  kind: 'claude',
  displayName: 'Docs writer',
  purpose: 'documentation',
  description: 'Writes flow documentation from the canvas',
  reportedContextWindow: 200_000,
  lastTest: {ok: false, error: 'deployment not found', at: '2026-09-29T10:00:00Z'},
})
const search = model('search0000', {
  deployment: 'text-embedding-3-large',
  kind: 'embedding',
  displayName: 'Search index',
})

function listing(connections: AzureConnection[], over: Partial<AzureConnectionsListing> = {}): AzureConnectionsListing {
  return {
    connections,
    registryState: 'ok',
    limits: {connections: 10, models: 50},
    knowledgeEmbeddingEnabled: false,
    ...over,
  }
}

function seed(
  connections: AzureConnection[] = [],
  models: AzureModelEntry[] = [],
  over: Partial<AzureConnectionsListing> & {serverDefault?: string} = {},
) {
  const {serverDefault, ...listingOver} = over
  api.listAzureConnections.mockResolvedValue(listing(connections, listingOver))
  api.listAzureModels.mockResolvedValue({models, ...(serverDefault ? {serverDefault} : {})})
}

function renderPanel(props: Partial<AzureFoundryPanelProps> = {}) {
  return render(
    <div>
      <h3 id={ANCHOR} tabIndex={-1}>
        Azure AI Foundry
      </h3>
      <AzureFoundryPanel scope={USER} headingLevel={4} anchorId={ANCHOR} {...props} />
    </div>,
  )
}

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return {promise, resolve, reject}
}

async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

function pick(menuLabel: string, item: string) {
  fireEvent.click(screen.getByRole('button', {name: menuLabel}))
  fireEvent.click(screen.getByRole('menuitem', {name: item}))
}

const status = () => document.querySelector<HTMLElement>('[role="status"].sr-only')!
const panelAlert = (container: HTMLElement) =>
  [...container.querySelectorAll<HTMLElement>('[role="alert"]')].find(el => !el.closest('[role="dialog"]'))!

const info = (sessionAnalytics: boolean): AppInfo => ({
  version: 'dev',
  platform: 'linux',
  arch: 'amd64',
  buildDate: '',
  gitCommit: '',
  capabilities: {sessionAnalytics},
})

beforeEach(async () => {
  vi.resetAllMocks()
  await resetAllStores()
  __resetPersistQueueForTest()
  useSettingsStore.setState({settings: initialSettings, updateProvider: realUpdateProvider})
  useSystemStore.setState({info: null, isLoaded: false})
  useUIStore.setState({settingsAnchor: null, settingsSection: null})
  confirmMock.mockResolvedValue(true)
  vi.mocked(settingsApi.updateSettings).mockResolvedValue(undefined)
  api.testAzure.mockResolvedValue({ok: true, latencyMs: 40})
  api.createAzureModel.mockImplementation(async (_scope, input) =>
    model(`n${input.deployment.replace(/[^a-z0-9]/gi, '').toLowerCase()}`.slice(0, 10).padEnd(10, '0'), {
      connectionId: input.connectionId,
      kind: input.kind,
      deployment: input.deployment,
      displayName: input.displayName ?? input.deployment,
    }),
  )
})

afterEach(() => {
  vi.useRealTimers()
})

describe('AzureFoundryPanel: states', () => {
  it('invites a first resource, says what it needs, and shows no toolbar', async () => {
    seed()
    renderPanel()
    expect(
      await screen.findByRole('heading', {level: 4, name: 'Connect an Azure AI Foundry resource'}),
    ).toBeInTheDocument()
    expect(screen.getByText(/Keys and Endpoint in the Foundry portal/)).toBeInTheDocument()
    expect(screen.getByText(/a project endpoint so baki can find them/)).toBeInTheDocument()
    expect(screen.getAllByRole('button', {name: 'Add resource'})).toHaveLength(1)
    expect(screen.queryByRole('button', {name: 'Add model'})).toBeNull()
  })

  it('shows a busy skeleton, then an alert with Retry — never the empty state — when the load fails', async () => {
    const connections = deferred<AzureConnectionsListing>()
    api.listAzureConnections.mockReturnValueOnce(connections.promise)
    api.listAzureModels.mockResolvedValue({models: []})
    const {container} = renderPanel()
    expect(container.querySelector('[aria-busy="true"]')).toHaveTextContent('Loading Azure AI Foundry resources…')

    await act(async () => connections.reject(new ApiError('boom', 500, 'INTERNAL')))
    await waitFor(() =>
      expect(panelAlert(container)).toHaveTextContent("Couldn't load the Azure AI Foundry resources and models."),
    )
    expect(screen.queryByText('Connect an Azure AI Foundry resource')).toBeNull()
    expect(container.querySelector('[aria-busy="true"]')).toBeNull()

    seed()
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
    expect(panelAlert(container)).toBeEmptyDOMElement()
  })

  it('keeps a loaded view when a refresh fails, and says so', async () => {
    seed([conn('default')], [gpt])
    const {container} = renderPanel()
    expect(await screen.findByText('GPT-4.1')).toBeInTheDocument()
    api.listAzureConnections.mockRejectedValueOnce(new Error('offline'))
    await act(async () => {
      await useAzureFoundryStore.getState().load(USER, {force: true})
    })
    expect(panelAlert(container)).toHaveTextContent(/Couldn't refresh the Azure AI Foundry resources/)
    expect(screen.getByText('GPT-4.1')).toBeInTheDocument()
  })

  it('gives a caller without access the note, not a dead Retry', async () => {
    api.listAzureConnections.mockRejectedValue(new ApiError('forbidden', 403, null))
    api.listAzureModels.mockResolvedValue({models: []})
    renderPanel({scope: ORG, headingLevel: 5, canEdit: false})
    expect(await screen.findByText(/Only organisation admins and members can use/)).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Retry'})).toBeNull()
  })

  it('renders nothing and tells the host when the deployment has no org keystore', async () => {
    api.listAzureConnections.mockRejectedValue(new ApiError('no keystore', 404, 'ORG_KEYSTORE_UNAVAILABLE'))
    api.listAzureModels.mockResolvedValue({models: []})
    const onUnavailable = vi.fn()
    const {container} = render(<AzureFoundryPanel scope={ORG} headingLevel={5} onUnavailable={onUnavailable} />)
    await waitFor(() => expect(onUnavailable).toHaveBeenCalledTimes(1))
    expect(container).toBeEmptyDOMElement()
  })

  it('an API-key resource without models explains how to add them by name', async () => {
    seed([conn('default')])
    renderPanel()
    expect(await screen.findByRole('heading', {level: 4, name: 'my-res'})).toBeInTheDocument()
    expect(screen.getByText("An API key can't list deployments. Add them by name.")).toBeInTheDocument()
    expect(screen.getByText(/my-res\.openai\.azure\.com · 0 models · Not tested/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Add model to my-res'}))
    expect(screen.getByRole('dialog', {name: 'Add model'})).toBeInTheDocument()
    expect(api.discoverAzureConnection).not.toHaveBeenCalled()
  })

  it('a project resource without models shows what discovery found right away', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue({
      deployments: [{name: 'o4-mini', kind: 'chat', model: 'o4-mini', contextWindow: 200000, alreadyAdded: false}],
      agents: [{name: 'Helper', kind: 'agent', model: 'gpt-4o', alreadyAdded: false}],
      warnings: [],
    })
    renderPanel()
    expect(await screen.findByText('Found in your project (2)')).toBeInTheDocument()
    const list = screen.getByRole('list', {name: 'Found in contoso'})
    expect(within(list).getByText('o4-mini')).toBeInTheDocument()
    expect(within(list).getByText(/Chat · 200k/)).toBeInTheDocument()
    expect(within(list).getByRole('button', {name: 'Add o4-mini (Chat)'})).toBeInTheDocument()
    expect(within(list).getByRole('button', {name: 'Add Helper (Agent)'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Hide suggestions from contoso'})).toHaveAttribute('aria-expanded', 'true')
  })

  it('a migrated project offers to review what it no longer lists, until it is dismissed', async () => {
    seed([entraConn('default', {name: 'my-res', suggestDiscovery: true, modelCount: 1})], [gpt])
    api.discoverAzureConnection.mockResolvedValue({
      deployments: [
        {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: gpt.modelId},
        {name: 'o4-mini', kind: 'chat', model: 'o4-mini', alreadyAdded: false},
      ],
      agents: [{name: 'Helper', kind: 'agent', model: '', alreadyAdded: false}],
      warnings: [],
    })
    api.updateAzureConnection.mockResolvedValue(entraConn('default', {name: 'my-res', modelCount: 1}))
    renderPanel()
    expect(await screen.findByText("Your project has 2 models you haven't added")).toBeInTheDocument()
    const review = screen.getByRole('button', {name: 'Review suggestions from my-res'})
    expect(review).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(review)
    expect(screen.getByRole('list', {name: 'Found in my-res'})).toBeInTheDocument()
    expect(within(screen.getByRole('list', {name: 'Found in my-res'})).getByText('Added')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Dismiss suggestions from my-res'}))
    await waitFor(() =>
      expect(api.updateAzureConnection).toHaveBeenCalledWith(USER, 'default', {dismissDiscovery: true}),
    )
    await waitFor(() => expect(status()).toHaveTextContent('Suggestions from my-res hidden'))
    expect(screen.queryByText("Your project has 2 models you haven't added")).toBeNull()
  })

  it('a resource needing credentials says so next to its models and offers Enter credentials', async () => {
    seed(
      [conn('default', {status: 'needs_credentials', statusReason: 'Its lender left the organisation.'})],
      [{...gpt, status: 'needs_credentials'}],
    )
    renderPanel()
    expect(await screen.findByText('Enter the credentials again')).toBeInTheDocument()
    expect(screen.getByText('Its models are unavailable in the chat until you enter them again.')).toBeInTheDocument()
    expect(screen.getByText('Its lender left the organisation.')).toBeInTheDocument()
    expect(screen.getByText("Unavailable until the resource's credentials are entered again")).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'More actions for GPT-4.1'}))
    const testItem = screen.getByRole('menuitem', {name: 'Test'})
    expect(testItem).toHaveAttribute('aria-disabled', 'true')
    expect(testItem).toHaveAccessibleDescription("Enter the resource's credentials first")
    fireEvent.click(testItem)
    expect(api.testAzure).not.toHaveBeenCalled()
    fireEvent.keyDown(screen.getByRole('menu'), {key: 'Escape'})

    fireEvent.click(screen.getByRole('button', {name: 'Enter credentials for my-res'}))
    expect(screen.getByRole('dialog', {name: 'Enter credentials for my-res'})).toBeInTheDocument()
  })

  it('re-enters the credentials of a resource whose settings came back blank, then focuses it', async () => {
    const blank = conn('default', {
      status: 'unreadable',
      config: {endpoint: '', authMode: '', hasSecret: false},
      modelCount: 1,
    })
    seed([blank], [{...gpt, status: 'unreadable'}])
    api.updateAzureConnection.mockResolvedValue(conn('default', {modelCount: 1}))
    renderPanel()
    const heading = await screen.findByRole('heading', {level: 4, name: 'my-res'})
    expect(screen.queryByText('API key')).toBeNull()
    expect(screen.getByText('1 model · Not tested')).toBeInTheDocument()

    const enter = screen.getByRole('button', {name: 'Enter credentials for my-res'})
    enter.focus()
    fireEvent.click(enter)
    const dialog = screen.getByRole('dialog', {name: 'Enter credentials for my-res'})
    expect(within(dialog).getByRole('radio', {name: 'API key'})).toHaveAttribute('aria-checked', 'true')
    fireEvent.change(within(dialog).getByRole('textbox', {name: 'Endpoint'}), {
      target: {value: 'https://my-res.openai.azure.com'},
    })
    fireEvent.change(within(dialog).getByLabelText('API key', {selector: 'input'}), {target: {value: 'sk-again'}})
    seed([conn('default', {modelCount: 1})], [gpt])
    fireEvent.click(within(dialog).getByRole('button', {name: 'Save credentials'}))

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(api.updateAzureConnection).toHaveBeenCalledWith(USER, 'default', {
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-again'}},
      updatedAt: '2026-09-30T10:00:00Z',
    })
    expect(screen.queryByText("The stored credentials can't be read")).toBeNull()
    await waitFor(() => expect(heading).toHaveFocus())
    expect(status()).toHaveTextContent('my-res saved')
    expect(screen.getByText(/my-res\.openai\.azure\.com · 1 model/)).toBeInTheDocument()
  })

  it('focuses the resource once Edit resource fixed its settings', async () => {
    seed([conn('default', {status: 'invalid_config', statusReason: 'The endpoint host is no longer allowed.'})], [gpt])
    api.updateAzureConnection.mockResolvedValue(conn('default', {name: 'my-res', modelCount: 1}))
    renderPanel()
    const edit = await screen.findByRole('button', {name: 'Edit resource my-res'})
    edit.focus()
    fireEvent.click(edit)
    const dialog = screen.getByRole('dialog', {name: 'Edit my-res'})
    fireEvent.change(within(dialog).getByRole('textbox', {name: 'Endpoint'}), {
      target: {value: 'https://my-res.services.ai.azure.com'},
    })
    fireEvent.change(within(dialog).getByLabelText('API key', {selector: 'input'}), {target: {value: 'sk-moved'}})
    seed([conn('default', {modelCount: 1})], [gpt])
    fireEvent.click(within(dialog).getByRole('button', {name: 'Save changes'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(screen.queryByRole('button', {name: 'Edit resource my-res'})).toBeNull()
    await waitFor(() => expect(screen.getByRole('heading', {level: 4, name: 'my-res'})).toHaveFocus())
  })

  it('members get the ask-an-admin variant without any action', async () => {
    seed([conn('default', {shared: true, status: 'needs_credentials', sharedBy: 'Ada'})], [{...gpt, shared: true}])
    renderPanel({scope: ORG, headingLevel: 5, canEdit: false})
    expect(
      await screen.findByText(
        'Its models are unavailable in the chat until an organisation admin enters the credentials again.',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /^Enter credentials/})).toBeNull()
  })

  it('an unreadable credential offers Enter credentials and Remove resource', async () => {
    seed([conn('default', {shared: true, status: 'unreadable'})])
    api.deleteAzureConnection.mockResolvedValue({status: 'ok', deletedModels: 0, clearedKnowledgeEmbedding: false})
    renderPanel({scope: ORG, headingLevel: 5, canEdit: true})
    expect(await screen.findByText("The stored credentials can't be read")).toBeInTheDocument()
    const callout = screen.getByText("The stored credentials can't be read").parentElement!
    expect(within(callout).getByRole('button', {name: 'Enter credentials for my-res'})).toBeInTheDocument()
    fireEvent.click(within(callout).getByRole('button', {name: 'Remove resource my-res'}))
    await waitFor(() => expect(api.deleteAzureConnection).toHaveBeenCalledWith(ORG, 'default', {cascade: false}))
    expect(confirmMock).toHaveBeenCalledWith(
      expect.objectContaining({title: 'Remove my-res?', confirmLabel: 'Remove resource', danger: true}),
    )
    await waitFor(() => expect(screen.getByRole('button', {name: 'Add resource'})).toHaveFocus())
  })

  it('never offers a member either way out', async () => {
    seed([conn('default', {shared: true, status: 'unreadable'})])
    renderPanel({scope: ORG, headingLevel: 5, canEdit: false})
    expect(await screen.findByText("The stored credentials can't be read")).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /^Enter credentials/})).toBeNull()
    expect(screen.queryByRole('button', {name: /^Remove resource/})).toBeNull()
  })

  it('shows an organisation read-only to a member, with who shared it and when', async () => {
    seed(
      [conn('default', {shared: true, sharedBy: 'Ada Admin', name: 'contoso'})],
      [{...gpt, shared: true, isDefault: true}],
    )
    renderPanel({scope: ORG, headingLevel: 5, canEdit: false})
    expect(await screen.findByRole('heading', {level: 5, name: 'contoso'})).toBeInTheDocument()
    expect(screen.getByText('Only organisation admins can change these resources and models.')).toBeInTheDocument()
    expect(screen.getByText(/Shared by Ada Admin/)).toBeInTheDocument()
    expect(screen.getByText(/^updated /).tagName).toBe('TIME')
    expect(screen.getByText('Default for members')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /Edit|More actions|Add/})).toBeNull()
  })

  it('lets an admin manage the organisation, with the member default as its own action', async () => {
    seed(
      [conn('default', {shared: true})],
      [
        {...gpt, shared: true},
        {...docs, shared: true, isDefault: true},
      ],
    )
    api.updateAzureModel.mockResolvedValue({...gpt, shared: true, isDefault: true})
    renderPanel({scope: ORG, headingLevel: 5, canEdit: true})
    await screen.findByText('GPT-4.1')
    expect(screen.getByRole('button', {name: 'Add resource'})).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'More actions for GPT-4.1'}))
    expect(screen.queryByRole('menuitem', {name: 'Make default'})).toBeNull()
    fireEvent.click(screen.getByRole('menuitem', {name: 'Make default for members'}))
    await waitFor(() => expect(api.updateAzureModel).toHaveBeenCalledWith(ORG, 'gpt4100000', {isDefault: true}))
    await waitFor(() => expect(status()).toHaveTextContent('GPT-4.1 is now the default for members'))
    const badges = screen.getAllByText('Default for members')
    expect(badges).toHaveLength(1)
    expect(badges[0]!.closest('li')).toHaveTextContent('GPT-4.1')
  })

  it('a list file from a newer baki is read-only, and a recovered one says where the copy is', async () => {
    seed([conn('default')], [gpt], {registryState: 'read_only'})
    const {unmount} = renderPanel()
    expect(await screen.findByText(/Update baki to change these models/)).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /Add resource|Add model|Edit|More actions/})).toBeNull()
    unmount()

    await resetAllStores()
    seed([], [], {registryState: 'recovered', recoveredCopyPath: 'C:\\baki\\azure-models.json.bad'})
    renderPanel()
    expect(
      await screen.findByText(/a copy was kept at C:\\baki\\azure-models\.json\.bad\. Add your models again\./),
    ).toBeInTheDocument()
  })

  it('invalid settings get a callout with Edit resource, and an unsupported agent says why', async () => {
    seed(
      [conn('default', {status: 'invalid_config', statusReason: 'The endpoint host is no longer allowed.'})],
      [
        {...gpt, status: 'invalid_config'},
        model('helper0000', {kind: 'agent', deployment: 'Helper', displayName: 'Helper', status: 'unsupported'}),
      ],
    )
    renderPanel()
    expect(await screen.findByText("This resource's settings are no longer accepted")).toBeInTheDocument()
    expect(screen.getByText('The endpoint host is no longer allowed.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Edit resource my-res'}))
    expect(screen.getByRole('dialog', {name: 'Edit my-res'})).toBeInTheDocument()
    expect(
      screen.getByText('Unavailable: agents need Microsoft Entra ID and a project endpoint on their resource'),
    ).toBeInTheDocument()
  })

  it('renders a resource and models a newer server describes with values this app does not know', async () => {
    seed(
      [
        conn('default', {
          status: unknownValue('suspended'),
          statusReason: 'Paused by your Azure administrator.',
          config: {
            endpoint: 'https://my-res.openai.azure.com',
            authMode: unknownValue('managed_identity'),
            hasSecret: false,
          },
        }),
      ],
      [
        gpt,
        model('voice00000', {
          kind: unknownValue('realtime'),
          deployment: 'gpt-realtime',
          displayName: 'Voice',
          purpose: unknownValue('translation'),
          status: unknownValue('suspended'),
        }),
      ],
      {registryState: unknownValue('migrating')},
    )
    renderPanel()
    expect(await screen.findByText('This resource is unavailable')).toBeInTheDocument()
    expect(screen.getByText('Paused by your Azure administrator.')).toBeInTheDocument()
    expect(screen.getByText('Other sign-in')).toBeInTheDocument()

    const rows = within(screen.getByRole('list', {name: 'Models of my-res'})).getAllByRole('listitem')
    const voice = rows.find(r => r.textContent?.includes('Voice'))!
    expect(within(voice).getByText('Other')).toBeInTheDocument()
    expect(voice).toHaveTextContent('Purpose: translation')
    expect(voice).toHaveTextContent('Unavailable right now')
    const edit = within(voice).getByRole('button', {name: 'Edit Voice'})
    expect(edit).toBeDisabled()
    expect(edit).toHaveAttribute('title', "This version of the app can't edit this kind of model.")
    fireEvent.click(within(voice).getByRole('button', {name: 'More actions for Voice'}))
    expect(screen.queryByRole('menuitem', {name: 'Make default'})).toBeNull()
    expect(screen.getByRole('menuitem', {name: 'Remove'})).toBeInTheDocument()
    expect(
      within(rows.find(r => r.textContent?.includes('GPT-4.1'))!).getByRole('button', {name: 'Edit GPT-4.1'}),
    ).toBeEnabled()
  })
})

describe('AzureFoundryPanel: the model list', () => {
  it('lists each model with its badges, purpose, window and last test, cells named for screen readers', async () => {
    seed([conn('default')], [gpt, docs, {...search, isKnowledgeEmbedding: true}], {
      serverDefault: gpt.modelId,
      knowledgeEmbeddingEnabled: true,
    })
    renderPanel()
    const list = await screen.findByRole('list', {name: 'Models of my-res'})
    const rows = within(list).getAllByRole('listitem')
    expect(rows.map(r => r.querySelector('[data-focus^="model-name:"]')?.textContent)).toEqual([
      'GPT-4.1',
      'Search index',
      'Docs writer',
    ])
    const docsRow = rows[2]!
    expect(within(docsRow).getByText('Claude')).toBeInTheDocument()
    expect(within(docsRow).getByText('claude-sonnet-4-6').closest('p')).toHaveTextContent(
      'claude-sonnet-4-6 · Writes flow documentation from the canvas',
    )
    expect(docsRow).toHaveTextContent('Purpose: Documentation')
    expect(docsRow).toHaveTextContent('Context window: 200k')
    expect(docsRow).toHaveTextContent(/Last test: Failed/)
    expect(within(docsRow).getByText('deployment not found')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('Default')).toBeInTheDocument()
    expect(rows[0]!).toHaveTextContent('Default for new chats')
    expect(within(rows[1]!).getByText('KB')).toBeInTheDocument()
    expect(rows[1]!).toHaveTextContent('Used for the knowledge base')
    expect(rows[1]).toHaveTextContent(/Context window: —\s*none/)
    expect(document.querySelector('.azure-models-head')).toHaveAttribute('aria-hidden', 'true')
    expect(within(docsRow).getByRole('button', {name: 'Edit Docs writer'})).toBeInTheDocument()
    expect(within(docsRow).getByRole('button', {name: 'More actions for Docs writer'})).toBeInTheDocument()
  })

  it('marks the saved default for new chats over the server default and hides Make default on it', async () => {
    useSettingsStore.setState(s => ({
      settings: {
        ...s.settings,
        ai: {
          ...s.settings.ai,
          providers: {
            ...s.settings.ai.providers,
            'azure-foundry': {...s.settings.ai.providers['azure-foundry'], defaultModel: docs.modelId},
          },
        },
      },
    }))
    seed([conn('default')], [gpt, docs], {serverDefault: gpt.modelId})
    renderPanel()
    const list = await screen.findByRole('list', {name: 'Models of my-res'})
    const [gptRow, docsRow] = within(list).getAllByRole('listitem')
    expect(within(docsRow!).getByText('Default')).toBeInTheDocument()
    expect(within(gptRow!).queryByText('Default')).toBeNull()
    fireEvent.click(within(docsRow!).getByRole('button', {name: 'More actions for Docs writer'}))
    expect(screen.queryByRole('menuitem', {name: 'Make default'})).toBeNull()
  })

  it('moves [Default] to the new server default once the model it marked is removed', async () => {
    seed([conn('default')], [gpt, docs], {serverDefault: gpt.modelId})
    api.deleteAzureModel.mockResolvedValue({status: 'ok'})
    renderPanel()
    await screen.findByText('Docs writer')
    const rows = () => within(screen.getByRole('list', {name: 'Models of my-res'})).getAllByRole('listitem')
    expect(within(rows()[0]!).getByText('Default')).toBeInTheDocument()

    api.listAzureModels.mockResolvedValue({models: [docs], serverDefault: docs.modelId})
    pick('More actions for GPT-4.1', 'Remove')
    await waitFor(() => expect(rows()).toHaveLength(1))
    await waitFor(() => expect(within(rows()[0]!).getByText('Default')).toBeInTheDocument())
    expect(rows()[0]!).toHaveTextContent('Docs writer')
  })

  it('Make default saves the one field optimistically and announces it', async () => {
    const updateProvider = vi.fn(async (id: string, cfg: Record<string, unknown>) => {
      useSettingsStore.setState(s => ({
        settings: {
          ...s.settings,
          ai: {
            ...s.settings.ai,
            providers: {...s.settings.ai.providers, [id]: {...s.settings.ai.providers['azure-foundry'], ...cfg}},
          },
        },
      }))
      return true
    })
    useSettingsStore.setState({updateProvider} as never)
    seed([conn('default')], [gpt, docs], {serverDefault: gpt.modelId})
    renderPanel()
    await screen.findByText('Docs writer')
    pick('More actions for Docs writer', 'Make default')
    expect(updateProvider).toHaveBeenCalledWith('azure-foundry', {defaultModel: 'm/docs000000'})
    await waitFor(() => expect(status()).toHaveTextContent('Docs writer is now the default for new chats'))
    const rows = within(screen.getByRole('list', {name: 'Models of my-res'})).getAllByRole('listitem')
    expect(within(rows[1]!).getByText('Default')).toBeInTheDocument()
    expect(within(rows[0]!).queryByText('Default')).toBeNull()
  })

  it('Make default rolls back only its field when the save fails, and says so', async () => {
    vi.mocked(settingsApi.updateSettings).mockRejectedValue(new Error('offline'))
    seed([conn('default')], [gpt, docs], {serverDefault: gpt.modelId})
    const {container} = renderPanel()
    await screen.findByText('Docs writer')
    vi.useFakeTimers()
    pick('More actions for Docs writer', 'Make default')
    const rows = () => within(screen.getByRole('list', {name: 'Models of my-res'})).getAllByRole('listitem')
    expect(within(rows()[1]!).getByText('Default')).toBeInTheDocument()
    void useSettingsStore.getState().updateProvider('azure-foundry', {temperature: 0.2})
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100)
    })
    vi.useRealTimers()
    await waitFor(() =>
      expect(panelAlert(container)).toHaveTextContent(
        "Couldn't make Docs writer the default for new chats; the previous default is kept.",
      ),
    )
    expect(within(rows()[0]!).getByText('Default')).toBeInTheDocument()
    expect(within(rows()[1]!).queryByText('Default')).toBeNull()
    const saved = useSettingsStore.getState().settings.ai.providers['azure-foundry']
    expect(saved.defaultModel).not.toBe('m/docs000000')
  })

  it('tests a row: a pass is announced politely, a failure as an alert with the reason', async () => {
    seed([conn('default')], [gpt, docs])
    const {container} = renderPanel()
    await screen.findByText('Docs writer')
    expect(status()).toBeEmptyDOMElement()
    expect(panelAlert(container)).toBeEmptyDOMElement()

    pick('More actions for GPT-4.1', 'Test')
    await waitFor(() => expect(status()).toHaveTextContent('GPT-4.1: test passed in 40 ms'))
    expect(api.testAzure).toHaveBeenCalledWith(USER, {model: {id: 'gpt4100000'}})
    expect(panelAlert(container)).toBeEmptyDOMElement()

    api.testAzure.mockResolvedValueOnce({ok: false, latencyMs: 0, error: 'rate limited'})
    pick('More actions for Docs writer', 'Test')
    await waitFor(() => expect(panelAlert(container)).toHaveTextContent('Docs writer: test failed. rate limited'))
    fireEvent.click(screen.getByRole('button', {name: 'Dismiss message'}))
    expect(panelAlert(container)).toBeEmptyDOMElement()
  })

  it('tests a resource, which costs nothing', async () => {
    seed([conn('default')], [gpt])
    renderPanel()
    await screen.findByText('GPT-4.1')
    pick('More actions for my-res', 'Test resource')
    await waitFor(() => expect(status()).toHaveTextContent('my-res: test passed in 40 ms'))
    expect(api.testAzure).toHaveBeenCalledWith(USER, {connectionId: 'default'})
  })

  it('offers Use for knowledge base only where the knowledge base exists, and confirms a re-index', async () => {
    seed([conn('default')], [gpt, search], {knowledgeEmbeddingEnabled: false})
    const first = renderPanel()
    await screen.findByText('Search index')
    fireEvent.click(screen.getByRole('button', {name: 'More actions for Search index'}))
    expect(screen.queryByRole('menuitem', {name: 'Use for knowledge base'})).toBeNull()
    expect(screen.queryByRole('menuitem', {name: 'Make default'})).toBeNull()
    first.unmount()

    await resetAllStores()
    seed([conn('default')], [gpt, search], {knowledgeEmbeddingEnabled: true})
    api.updateAzureModel
      .mockRejectedValueOnce(new VersionConflictError('re-index', 'REINDEX_REQUIRED'))
      .mockResolvedValueOnce({...search, isKnowledgeEmbedding: true})
    renderPanel()
    await screen.findByText('Search index')
    pick('More actions for Search index', 'Use for knowledge base')
    await waitFor(() => expect(api.updateAzureModel).toHaveBeenCalledTimes(2))
    expect(api.updateAzureModel.mock.calls[0]![2]).toEqual({isKnowledgeEmbedding: true})
    expect(api.updateAzureModel.mock.calls[1]![2]).toEqual({isKnowledgeEmbedding: true, force: true})
    expect(confirmMock).toHaveBeenCalledWith(expect.objectContaining({title: 'Change the knowledge base model?'}))
    await waitFor(() => expect(status()).toHaveTextContent(/now uses Search index\. Re-index it under Knowledge Base/))
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({user: true})
  })

  it('removes a model after a confirm and moves focus to the next row, the previous one, then Add model', async () => {
    seed([conn('default')], [gpt, model('mini000000', {deployment: 'o4-mini', displayName: 'o4-mini'}), docs])
    api.deleteAzureModel.mockResolvedValue({status: 'ok'})
    renderPanel()
    await screen.findByText('Docs writer')

    pick('More actions for o4-mini', 'Remove')
    await waitFor(() => expect(api.deleteAzureModel).toHaveBeenCalledWith(USER, 'mini000000', {}))
    expect(confirmMock).toHaveBeenCalledWith(
      expect.objectContaining({title: 'Remove o4-mini?', confirmLabel: 'Remove model', danger: true}),
    )
    await waitFor(() => expect(screen.getByRole('button', {name: 'Edit Docs writer'})).toHaveFocus())
    expect(status()).toHaveTextContent('o4-mini removed')

    pick('More actions for Docs writer', 'Remove')
    await waitFor(() => expect(screen.getByRole('button', {name: 'Edit GPT-4.1'})).toHaveFocus())

    pick('More actions for GPT-4.1', 'Remove')
    await waitFor(() => expect(screen.getByRole('button', {name: 'Add model to my-res'})).toHaveFocus())
  })

  it('keeps a model when the remove confirm is declined, and says why a removal failed', async () => {
    seed([conn('default')], [gpt])
    const {container} = renderPanel()
    await screen.findByText('GPT-4.1')
    confirmMock.mockResolvedValueOnce(false)
    pick('More actions for GPT-4.1', 'Remove')
    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(api.deleteAzureModel).not.toHaveBeenCalled()

    api.deleteAzureModel.mockRejectedValueOnce(new ApiError('forbidden', 403, null))
    pick('More actions for GPT-4.1', 'Remove')
    await waitFor(() =>
      expect(panelAlert(container)).toHaveTextContent(
        "Couldn't remove GPT-4.1: Only organisation admins can change the shared resources and models.",
      ),
    )
    expect(screen.getByText('GPT-4.1')).toBeInTheDocument()
  })

  it('turns read-only when a change finds a list file from a newer baki', async () => {
    seed([conn('default')], [gpt])
    api.deleteAzureModel.mockRejectedValueOnce(new VersionConflictError('read-only', 'REGISTRY_READ_ONLY'))
    const {container} = renderPanel()
    await screen.findByText('GPT-4.1')
    seed([conn('default')], [gpt], {registryState: 'read_only'})
    pick('More actions for GPT-4.1', 'Remove')
    await waitFor(() =>
      expect(panelAlert(container)).toHaveTextContent(
        "Couldn't remove GPT-4.1: This list was saved by a newer version of baki.",
      ),
    )
    await waitFor(() => expect(screen.queryByRole('button', {name: 'More actions for GPT-4.1'})).toBeNull())
    expect(screen.getAllByText(/Update baki to change these models/)).toHaveLength(2)
  })

  it('removing the knowledge-base model warns about the re-index, and forces it only when the server asks', async () => {
    seed([conn('default')], [gpt, {...search, isKnowledgeEmbedding: true}], {knowledgeEmbeddingEnabled: true})
    api.deleteAzureModel
      .mockRejectedValueOnce(new VersionConflictError('re-index', 'REINDEX_REQUIRED'))
      .mockResolvedValueOnce({status: 'ok'})
    renderPanel()
    await screen.findByText('Search index')
    expect(screen.getByText('KB')).toBeInTheDocument()
    pick('More actions for Search index', 'Remove')
    await waitFor(() => expect(api.deleteAzureModel).toHaveBeenCalledTimes(2))
    expect(api.deleteAzureModel.mock.calls[0]![2]).toEqual({})
    expect(api.deleteAzureModel.mock.calls[1]![2]).toEqual({force: true})
    expect(confirmMock).toHaveBeenCalledTimes(1)
    const {container} = render(<>{confirmMock.mock.calls[0]![0].message}</>)
    expect(container).toHaveTextContent('The knowledge base must be re-indexed afterwards')
    await waitFor(() => expect(status()).toHaveTextContent('Search index removed'))
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({user: true})
  })

  it('removes the knowledge-base model of an empty corpus without forcing a re-index', async () => {
    seed([conn('default')], [gpt, {...search, isKnowledgeEmbedding: true}], {knowledgeEmbeddingEnabled: true})
    api.deleteAzureModel.mockResolvedValue({status: 'ok'})
    renderPanel()
    await screen.findByText('Search index')
    pick('More actions for Search index', 'Remove')
    await waitFor(() => expect(status()).toHaveTextContent('Search index removed'))
    expect(api.deleteAzureModel).toHaveBeenCalledTimes(1)
    expect(api.deleteAzureModel).toHaveBeenCalledWith(USER, 'search0000', {})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
  })

  it('shows and forces nothing about the knowledge base where none exists', async () => {
    seed([conn('default', {modelCount: 2})], [gpt, {...search, isKnowledgeEmbedding: true}], {
      knowledgeEmbeddingEnabled: false,
    })
    api.deleteAzureModel.mockResolvedValue({status: 'ok'})
    api.deleteAzureConnection.mockResolvedValue({status: 'ok', deletedModels: 2, clearedKnowledgeEmbedding: true})
    const first = renderPanel()
    const row = (await screen.findByText('Search index')).closest('li')!
    expect(within(row).queryByText('KB')).toBeNull()
    expect(row).not.toHaveTextContent('Used for the knowledge base')

    pick('More actions for my-res', 'Remove resource')
    await waitFor(() => expect(api.deleteAzureConnection).toHaveBeenCalledWith(USER, 'default', {cascade: true}))
    const {container: resourceMessage} = render(<>{confirmMock.mock.calls[0]![0].message}</>)
    expect(resourceMessage).not.toHaveTextContent('re-indexed')
    first.unmount()

    await resetAllStores()
    confirmMock.mockClear()
    renderPanel()
    await screen.findByText('Search index')
    pick('More actions for Search index', 'Remove')
    await waitFor(() => expect(api.deleteAzureModel).toHaveBeenCalledWith(USER, 'search0000', {}))
    const {container: modelMessage} = render(<>{confirmMock.mock.calls[0]![0].message}</>)
    expect(modelMessage).not.toHaveTextContent('re-indexed')
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
  })
})

describe('AzureFoundryPanel: resources', () => {
  it('asks before removing a resource, naming its models, and removes them with it', async () => {
    seed([conn('default'), conn('second0000', {name: 'contoso'})], [gpt, docs])
    api.deleteAzureConnection.mockResolvedValue({status: 'ok', deletedModels: 2, clearedKnowledgeEmbedding: false})
    renderPanel()
    await screen.findByText('Docs writer')
    pick('More actions for my-res', 'Remove resource')
    await waitFor(() => expect(api.deleteAzureConnection).toHaveBeenCalledWith(USER, 'default', {cascade: true}))
    const opts = confirmMock.mock.calls[0]![0]
    expect(opts).toMatchObject({title: 'Remove my-res?', confirmLabel: 'Remove resource and 2 models', danger: true})
    const {container} = render(<>{opts.message}</>)
    expect(container).toHaveTextContent(
      "deletes the resource's stored credentials and its 2 models: GPT-4.1, Docs writer",
    )
    await waitFor(() => expect(screen.getByRole('heading', {name: 'contoso'})).toHaveFocus())
    expect(status()).toHaveTextContent('my-res removed')
  })

  it('shows models the view did not know of when the server refuses (in use)', async () => {
    seed([conn('default')])
    api.deleteAzureConnection.mockRejectedValueOnce(new VersionConflictError('has 2 models', 'CONNECTION_IN_USE'))
    const {container} = renderPanel()
    await screen.findByRole('heading', {name: 'my-res'})
    const loads = api.listAzureConnections.mock.calls.length
    pick('More actions for my-res', 'Remove resource')
    await waitFor(() => expect(panelAlert(container)).toHaveTextContent("Couldn't remove my-res: has 2 models"))
    await waitFor(() => expect(api.listAzureConnections.mock.calls.length).toBe(loads + 1))
  })

  it('focuses a new resource and opens its suggestions', async () => {
    seed()
    api.createAzureConnection.mockImplementation(async (_scope, input) => entraConn('contoso000', {name: input.name}))
    api.discoverAzureConnection.mockResolvedValue({
      deployments: [{name: 'o4-mini', kind: 'chat', model: 'o4-mini', alreadyAdded: false}],
      agents: [],
      warnings: [],
    })
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Add resource'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Endpoint'}), {
      target: {value: 'https://contoso.openai.azure.com'},
    })
    fireEvent.change(screen.getByLabelText('API key', {selector: 'input'}), {target: {value: 'sk'}})
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Add resource'}))
    const heading = await screen.findByRole('heading', {level: 4, name: 'contoso'})
    await waitFor(() => expect(heading).toHaveFocus())
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(status()).toHaveTextContent('contoso added')
    expect(await screen.findByRole('list', {name: 'Found in contoso'})).toBeInTheDocument()
  })

  it('returns focus to the trigger when a modal is cancelled', async () => {
    seed([conn('default')], [gpt])
    renderPanel()
    const add = await screen.findByRole('button', {name: 'Add model to my-res'})
    add.focus()
    fireEvent.click(add)
    await flushFrame()
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(add).toHaveFocus()
  })

  it("focuses a new model's name once it is added", async () => {
    seed([conn('default')], [gpt])
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Add model to my-res'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Deployment name'}), {target: {value: 'o4-mini'}})
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Add model'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const name = screen.getAllByText('o4-mini').find(el => el.dataset.focus?.startsWith('model-name:'))!
    await waitFor(() => expect(name).toHaveFocus())
    expect(status()).toHaveTextContent('o4-mini added')
  })

  it('offers the Azure CLI only for a personal resource on a local backend', async () => {
    useSystemStore.setState({info: info(true), isLoaded: true})
    seed()
    const personal = renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Add resource'}))
    expect(screen.getByRole('radio', {name: 'Azure CLI'})).toBeInTheDocument()
    personal.unmount()

    seed()
    render(<AzureFoundryPanel scope={ORG} headingLevel={5} canEdit />)
    fireEvent.click(await screen.findByRole('button', {name: 'Add resource'}))
    expect(screen.queryByRole('radio', {name: 'Azure CLI'})).toBeNull()
  })
})

describe('AzureFoundryPanel: discovery', () => {
  const found = {
    deployments: [
      {name: 'o4-mini', kind: 'chat' as const, model: 'o4-mini', alreadyAdded: false},
      {name: 'phi-4', kind: 'chat' as const, model: 'phi-4', alreadyAdded: false},
      {
        name: 'text-embedding-3-small',
        kind: 'embedding' as const,
        model: 'text-embedding-3-small',
        alreadyAdded: false,
      },
    ],
    agents: [],
    warnings: ['Agents could not be listed: the caller lacks the Azure AI User role.'],
  }

  it('quick-adds with the defaults, flips the row to Added and reports the first chat model', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue(found)
    const onFirstChatModel = vi.fn()
    renderPanel({onFirstChatModel})
    const list = await screen.findByRole('list', {name: 'Found in contoso'})
    expect(screen.getByText(/lacks the Azure AI User role/)).toBeInTheDocument()

    fireEvent.click(within(list).getByRole('button', {name: 'Add o4-mini (Chat)'}))
    await waitFor(() => expect(api.createAzureModel).toHaveBeenCalledTimes(1))
    expect(api.createAzureModel).toHaveBeenCalledWith(USER, {
      connectionId: 'contoso',
      kind: 'chat',
      deployment: 'o4-mini',
      purpose: 'general',
    })
    const row = within(list).getByText('o4-mini').closest('li')!
    await waitFor(() => expect(within(row).getByText('Added')).toBeInTheDocument())
    expect(onFirstChatModel).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(within(list).getByRole('button', {name: 'Add phi-4 (Chat)'})).toHaveFocus())

    fireEvent.click(within(list).getByRole('button', {name: 'Add phi-4 (Chat)'}))
    await waitFor(() => expect(api.createAzureModel).toHaveBeenCalledTimes(2))
    expect(onFirstChatModel).toHaveBeenCalledTimes(1)
  })

  it('announces a failed quick add and puts focus back on its Add, which the reason describes', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue(found)
    const pending = deferred<AzureModelEntry>()
    api.createAzureModel.mockReturnValueOnce(pending.promise)
    const {container} = renderPanel()
    const list = await screen.findByRole('list', {name: 'Found in contoso'})
    const add = within(list).getByRole('button', {name: 'Add phi-4 (Chat)'})
    add.focus()
    fireEvent.click(add)
    expect(add).toBeDisabled()
    const elsewhere = screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})
    act(() => elsewhere.focus())
    expect(elsewhere).toHaveFocus()

    await act(async () => pending.reject(new ApiError('deployment name is invalid', 400, 'INVALID_DEPLOYMENT')))
    await waitFor(() =>
      expect(panelAlert(container)).toHaveTextContent("Couldn't add phi-4: deployment name is invalid"),
    )
    const again = within(list).getByRole('button', {name: 'Add phi-4 (Chat)'})
    await waitFor(() => expect(again).toHaveFocus())
    expect(again).toHaveAccessibleDescription('Not added: deployment name is invalid')
    expect(status()).toBeEmptyDOMElement()
  })

  it("names each resource's repeated buttons after it, keeping their visible labels", async () => {
    seed([entraConn('contoso'), entraConn('fabrikam')])
    api.discoverAzureConnection.mockResolvedValue(found)
    renderPanel()
    for (const name of ['contoso', 'fabrikam']) {
      expect(await screen.findByRole('button', {name: `Hide suggestions from ${name}`})).toHaveTextContent('Hide')
      expect(screen.getByRole('button', {name: `Add all suggestions from ${name}`})).toHaveTextContent('Add all')
      expect(screen.getByRole('button', {name: `Refresh suggestions from ${name}`})).toHaveTextContent('Refresh')
    }
    fireEvent.click(screen.getByRole('button', {name: 'Hide suggestions from fabrikam'}))
    expect(screen.getByRole('button', {name: 'Show suggestions from fabrikam'})).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.getByRole('button', {name: 'Hide suggestions from contoso'})).toHaveAttribute('aria-expanded', 'true')
  })

  it('shows a quick-add failure on its own row', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue(found)
    api.createAzureModel.mockRejectedValueOnce(new ApiError('deployment name is invalid', 400, 'INVALID_DEPLOYMENT'))
    renderPanel()
    const list = await screen.findByRole('list', {name: 'Found in contoso'})
    fireEvent.click(within(list).getByRole('button', {name: 'Add phi-4 (Chat)'}))
    const row = within(list).getByText('phi-4').closest('li')!
    expect(await within(row).findByText('Not added: deployment name is invalid')).toBeInTheDocument()
    expect(within(row).getByRole('button', {name: 'Add phi-4 (Chat)'})).toBeInTheDocument()
  })

  it('adds all, one after the other, and stops at the model limit', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue(found)
    const {container} = renderPanel()
    const list = await screen.findByRole('list', {name: 'Found in contoso'})
    api.createAzureModel
      .mockImplementationOnce(async (_s, input) =>
        model('added00000', {connectionId: 'contoso', deployment: input.deployment}),
      )
      .mockRejectedValueOnce(new VersionConflictError('limit 50', 'TOO_MANY_MODELS'))
    fireEvent.click(screen.getByRole('button', {name: 'Add all suggestions from contoso'}))
    await waitFor(() => expect(status()).toHaveTextContent('Added 1 model'))
    expect(api.createAzureModel).toHaveBeenCalledTimes(2)
    expect(panelAlert(container)).toHaveTextContent("2 models couldn't be added; the list says why.")
    expect(
      within(within(list).getByText('text-embedding-3-small').closest('li')!).getByText('Not added: limit 50'),
    ).toBeInTheDocument()
  })

  it('opens the pre-filled Model modal from "Add with details…"', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValue(found)
    renderPanel()
    const list = await screen.findByRole('list', {name: 'Found in contoso'})
    fireEvent.click(within(list).getByRole('button', {name: 'Add with details: text-embedding-3-small'}))
    const dialog = screen.getByRole('dialog', {name: 'Add model'})
    expect(within(dialog).getByRole('textbox', {name: 'Deployment name'})).toHaveValue('text-embedding-3-small')
    expect(within(dialog).getByRole('radio', {name: 'Embedding'})).toHaveAttribute('aria-checked', 'true')
  })

  it('shows why discovery failed with Retry, and that adding by name still works', async () => {
    seed([entraConn('contoso')])
    api.discoverAzureConnection.mockResolvedValueOnce({
      deployments: [],
      agents: [],
      warnings: [],
      error: 'Azure answered 403',
    })
    renderPanel()
    expect(
      await screen.findByText("Couldn't list this project's deployments and agents: Azure answered 403"),
    ).toBeInTheDocument()
    expect(screen.getByText('You can still add models by name.')).toBeInTheDocument()
    api.discoverAzureConnection.mockResolvedValueOnce(found)
    fireEvent.click(screen.getByRole('button', {name: 'Retry finding deployments and agents in contoso'}))
    expect(await screen.findByRole('list', {name: 'Found in contoso'})).toBeInTheDocument()
  })
})

describe('AzureFoundryPanel: limits', () => {
  it('shows the counts from 80 % and disables Add at the limit, saying why', async () => {
    const eight = Array.from({length: 8}, (_, i) => conn(`res${i}000000`.slice(0, 10), {name: `res-${i}`}))
    seed(eight, [], {limits: {connections: 10, models: 50}})
    const {unmount} = renderPanel()
    expect(await screen.findByText('8 of 10 resources · 0 of 50 models')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Add resource'})).toBeEnabled()
    unmount()

    await resetAllStores()
    seed(eight, [gpt], {limits: {connections: 8, models: 1}})
    renderPanel()
    const add = await screen.findByRole('button', {name: 'Add resource'})
    expect(add).toBeDisabled()
    expect(add).toHaveAccessibleDescription("You've reached the limit of 8 resources. Remove one to add another.")
    const addModel = screen.getAllByRole('button', {name: 'Add model'})[0]
    expect(addModel).toBeDisabled()
    expect(addModel).toHaveAccessibleDescription("You've reached the limit of 1 models. Remove one to add another.")
    expect(screen.getByRole('button', {name: 'Add model to res-0'})).toBeDisabled()
  })
})

describe('AzureFoundryPanel: Shared with you', () => {
  const acme: Organisation = {
    id: 'org-1',
    name: 'Contoso',
    ownerId: 'u-admin',
    members: [
      {userId: 'u-admin', role: 'admin', joinedAt: '2024-01-01T00:00:00Z'},
      {userId: 'u-2', role: 'member', joinedAt: '2024-01-01T00:00:00Z'},
    ],
    createdAt: '2024-01-01T00:00:00Z',
    updatedAt: '2024-01-01T00:00:00Z',
  }
  const sharedConn = conn('default', {shared: true, orgId: 'org-1', orgName: 'Contoso', name: 'contoso-prod'})
  const review = model('review0000', {
    shared: true,
    orgId: 'org-1',
    orgName: 'Contoso',
    connectionName: 'contoso-prod',
    kind: 'agent',
    deployment: 'reviewer',
    displayName: 'Review bot',
    purpose: 'review',
    isDefault: true,
  })

  it("groups an organisation's models read-only, each wearing the Shared badge", async () => {
    useOrgStore.setState({organisations: [acme]} as never)
    useAuthStore.setState({user: {id: 'u-2', email: 'bob@contoso.io'}} as never)
    seed([sharedConn], [review])
    renderPanel()
    expect(await screen.findByRole('heading', {level: 4, name: 'Shared with you by Contoso'})).toBeInTheDocument()
    const list = screen.getByRole('list', {name: 'Shared with you by Contoso'})
    const row = within(list).getByRole('listitem')
    expect(within(row).getByText('Shared')).toBeInTheDocument()
    expect(row).toHaveTextContent('Shared by Contoso')
    expect(row).toHaveTextContent('on contoso-prod')
    expect(within(row).getByText('Default for members')).toBeInTheDocument()
    expect(row).toHaveTextContent(
      "used in this organisation's flows when you have no Azure models or default of your own",
    )
    expect(within(row).queryByRole('button')).toBeNull()
    expect(screen.queryByRole('button', {name: 'Manage in Organizations'})).toBeNull()
    expect(screen.getByText('Connect an Azure AI Foundry resource')).toBeInTheDocument()
  })

  it("sends an admin to the organisation's block in Organizations", async () => {
    useOrgStore.setState({organisations: [acme]} as never)
    useAuthStore.setState({user: {id: 'u-admin', email: 'ada@contoso.io'}} as never)
    seed([sharedConn], [review])
    renderPanel()
    const manage = await screen.findByRole('button', {name: 'Manage in Organizations'})
    expect(manage).toHaveAccessibleDescription('Shared with you by Contoso')
    fireEvent.click(manage)
    expect(useUIStore.getState()).toMatchObject({
      settingsOpen: true,
      settingsSection: 'orgs',
      settingsAnchor: 'org-azure-foundry-org-1',
    })
  })
})

describe('AzureFoundryPanel: deep links', () => {
  it('focuses the anchored heading once the data has loaded, then spends the anchor', async () => {
    const connections = deferred<AzureConnectionsListing>()
    api.listAzureConnections.mockReturnValueOnce(connections.promise)
    api.listAzureModels.mockResolvedValue({models: []})
    useUIStore.setState({settingsAnchor: ANCHOR})
    renderPanel()
    const heading = screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})
    expect(heading).not.toHaveFocus()
    expect(useUIStore.getState().settingsAnchor).toBe(ANCHOR)

    await act(async () => connections.resolve(listing([conn('default')])))
    await waitFor(() => expect(heading).toHaveFocus())
    expect(useUIStore.getState().settingsAnchor).toBeNull()
  })

  it("leaves another panel's anchor alone", async () => {
    seed()
    useUIStore.setState({settingsAnchor: 'org-azure-foundry-org-1'})
    renderPanel()
    await screen.findByText('Connect an Azure AI Foundry resource')
    expect(useUIStore.getState().settingsAnchor).toBe('org-azure-foundry-org-1')
  })

  it('also follows an anchor set while the panel is already shown', async () => {
    seed()
    renderPanel()
    await screen.findByText('Connect an Azure AI Foundry resource')
    act(() => useUIStore.getState().openSettings('accounts', ANCHOR))
    await waitFor(() => expect(screen.getByRole('heading', {level: 3, name: 'Azure AI Foundry'})).toHaveFocus())
    expect(useUIStore.getState().settingsAnchor).toBeNull()
  })
})
