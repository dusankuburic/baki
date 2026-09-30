import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import AzureModelModal from './AzureModelModal'
import {emptyModelForm, modelFormFromDiscovery, modelFormFromEntry, type ModelFormState} from './modelForm'
import {providersApi} from '@/api'
import {ApiError, VersionConflictError} from '@/api/client'
import {resetAllStores} from '@/stores/storeRegistry'
import {useAzureFoundryStore} from '@/stores/azureFoundryStore'
import type {AzureConnection, AzureModelEntry, AzureScope} from '@/types'

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
      discoverAzureConnection: vi.fn(),
      createAzureModel: vi.fn(),
      updateAzureModel: vi.fn(),
      testAzure: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)
const USER: AzureScope = {kind: 'user'}

function conn(id: string, over: Partial<AzureConnection> = {}): AzureConnection {
  return {
    id,
    name: id === 'default' ? 'my-res' : id,
    shared: false,
    legacy: id === 'default',
    config: {endpoint: `https://${id}.openai.azure.com`, authMode: 'api_key', hasSecret: true},
    status: 'ready',
    canDiscover: false,
    supportsAgents: false,
    suggestDiscovery: false,
    modelCount: 0,
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

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

const docs = model('docs000000', {
  deployment: 'claude-sonnet-4-6',
  kind: 'claude',
  displayName: 'Docs writer',
  purpose: 'documentation',
  description: 'Writes flow documentation',
  contextWindow: 200000,
})

function renderModal(
  opts: {
    entry?: AzureModelEntry | null
    initial?: ModelFormState
    connections?: AzureConnection[]
    models?: AzureModelEntry[]
    reportedWindow?: number
  } = {},
) {
  const onClose = vi.fn()
  const onSaved = vi.fn()
  const entry = opts.entry ?? null
  const utils = render(
    <AzureModelModal
      scope={USER}
      entry={entry}
      initial={opts.initial ?? (entry ? modelFormFromEntry(entry) : emptyModelForm('default'))}
      reportedWindow={opts.reportedWindow}
      connections={opts.connections ?? [conn('default')]}
      models={opts.models ?? []}
      onClose={onClose}
      onSaved={onSaved}
    />,
  )
  return {...utils, onClose, onSaved}
}

async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

const deployment = () => screen.getByRole('textbox', {name: 'Deployment name'})
const displayName = () => screen.getByRole('textbox', {name: 'Display name'})
const contextWindow = () => screen.getByRole('textbox', {name: 'Context window (tokens, optional)'})
const maxOutput = () => screen.getByRole('textbox', {name: 'Max output tokens (optional)'})
const submit = (name = 'Add model') => fireEvent.click(screen.getByRole('button', {name}))

beforeEach(async () => {
  vi.resetAllMocks()
  await resetAllStores()
  confirmMock.mockResolvedValue(true)
  api.createAzureModel.mockImplementation(async (_scope, input) =>
    model('new0000000', {
      deployment: input.deployment,
      kind: input.kind,
      displayName: input.displayName ?? input.deployment,
    }),
  )
  api.updateAzureModel.mockImplementation(async (_scope, id, input) =>
    model(id, {deployment: input.deployment, displayName: input.displayName}),
  )
  api.testAzure.mockResolvedValue({ok: true, latencyMs: 40})
})

describe('AzureModelModal: the form', () => {
  it('opens on the name with the resource as text when there is only one, full screen below 640 px', async () => {
    renderModal()
    const dialog = screen.getByRole('dialog', {name: 'Add model'})
    expect(dialog.className).toContain('max-sm:h-full')
    expect(screen.queryByRole('combobox', {name: 'Resource'})).toBeNull()
    expect(within(dialog).getByText('my-res')).toBeInTheDocument()
    await flushFrame()
    expect(deployment()).toHaveFocus()

    const save = screen.getByRole('button', {name: 'Add model'})
    const form = dialog.querySelector('form')!
    expect(save).toHaveAttribute('type', 'submit')
    expect(save).toHaveAttribute('form', form.id)
    expect(form).toHaveAttribute('novalidate')
  })

  it('offers the resources as a select, starting on the one it was opened from', async () => {
    renderModal({
      connections: [conn('default'), conn('second0000', {name: 'contoso-prod'})],
      initial: emptyModelForm('second0000'),
    })
    const select = screen.getByRole('combobox', {name: 'Resource'})
    expect(select).toHaveValue('second0000')
    await flushFrame()
    expect(select).toHaveFocus()
  })

  it('lets the display name follow the deployment name until it is edited', () => {
    renderModal()
    fireEvent.change(deployment(), {target: {value: 'gpt-4.1'}})
    expect(displayName()).toHaveValue('gpt-4.1')
    fireEvent.change(displayName(), {target: {value: 'Everyday'}})
    fireEvent.change(deployment(), {target: {value: 'gpt-4.1-mini'}})
    expect(displayName()).toHaveValue('Everyday')
  })

  it('offers agents only on a resource with Entra ID and a project, and says why', () => {
    renderModal()
    const agent = screen.getByRole('radio', {name: 'Agent'})
    expect(agent).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByRole('radiogroup', {name: 'Kind'})).toHaveAccessibleDescription(
      'Agents need a resource with Microsoft Entra ID and a project endpoint.',
    )
    fireEvent.click(agent)
    expect(screen.getByRole('radio', {name: 'Chat'})).toHaveAttribute('aria-checked', 'true')
  })

  it('names an agent by its agent name and says the test does not run it', () => {
    renderModal({connections: [conn('default', {supportsAgents: true, canDiscover: true})]})
    fireEvent.click(screen.getByRole('radio', {name: 'Agent'}))
    expect(screen.getByRole('textbox', {name: 'Agent name'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Test model'})).toHaveAccessibleDescription(
      "Checks that the agent exists; it isn't run.",
    )
    fireEvent.click(screen.getByRole('radio', {name: 'Chat'}))
    expect(screen.getByRole('button', {name: 'Test model'})).toHaveAccessibleDescription(
      'Sends one tiny request; Azure bills it.',
    )
  })

  it('suggests the kind a name hints at, without switching by itself', () => {
    renderModal()
    fireEvent.change(deployment(), {target: {value: 'my-claude-sonnet'}})
    expect(screen.getByText('The name suggests a Claude model.')).toBeInTheDocument()
    expect(screen.getByRole('radio', {name: 'Chat'})).toHaveAttribute('aria-checked', 'true')
    fireEvent.click(screen.getByRole('button', {name: 'Switch to Claude'}))
    expect(screen.getByRole('radio', {name: 'Claude'})).toHaveAttribute('aria-checked', 'true')
    expect(screen.queryByText('The name suggests a Claude model.')).toBeNull()
  })

  it('hides the advanced numbers for an embedding and shows the reported window otherwise', () => {
    renderModal({reportedWindow: 128000})
    expect(contextWindow()).toHaveAttribute('placeholder', '128000')
    expect(contextWindow()).toHaveAccessibleDescription(/Reported: 128,000 tokens/)
    fireEvent.click(screen.getByRole('radio', {name: 'Embedding'}))
    expect(screen.queryByRole('textbox', {name: 'Context window (tokens, optional)'})).toBeNull()
  })

  it('says when no window is known', () => {
    renderModal()
    expect(contextWindow()).toHaveAccessibleDescription(
      'Not detected: baki assumes 128k; set it if your model is smaller.',
    )
  })

  it('takes a custom purpose, counts the description and posts a clean body', async () => {
    const {onSaved} = renderModal()
    fireEvent.change(deployment(), {target: {value: ' phi-4-mini '}})
    fireEvent.change(screen.getByRole('combobox', {name: 'Purpose'}), {target: {value: 'custom'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Custom purpose'}), {target: {value: '  Security   review '}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {target: {value: 'Distilled'}})
    expect(screen.getByText('9/200')).toBeInTheDocument()
    fireEvent.change(maxOutput(), {target: {value: '2048'}})
    submit()
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(expect.objectContaining({id: 'new0000000'}), true))
    expect(api.createAzureModel.mock.calls[0][1]).toStrictEqual({
      connectionId: 'default',
      kind: 'chat',
      deployment: 'phi-4-mini',
      purpose: 'custom',
      purposeCustom: 'Security review',
      displayName: 'phi-4-mini',
      description: 'Distilled',
      maxOutputTokens: 2048,
    })
  })

  // Optional (b): the server collapses whitespace runs before it counts.
  it('counts the description as the server stores it', () => {
    renderModal()
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {
      target: {value: '  Writes   docs\n\nfast  '},
    })
    // "Writes docs fast"
    expect(screen.getByText('16/200')).toBeInTheDocument()
  })
})

describe('AzureModelModal: validation and focus', () => {
  it('validates on submit, focusing the first invalid field, and checks formats on blur', async () => {
    renderModal()
    submit()
    expect(deployment()).toHaveFocus()
    expect(deployment()).toHaveAccessibleDescription('Required')
    expect(api.createAzureModel).not.toHaveBeenCalled()

    fireEvent.change(deployment(), {target: {value: 'bad name!'}})
    expect(deployment()).toHaveAccessibleDescription(/Use letters, digits/)
    fireEvent.change(deployment(), {target: {value: 'gpt-4.1'}})
    expect(deployment()).not.toHaveAttribute('aria-invalid')
  })

  it('opens Advanced to focus an invalid number inside it', async () => {
    renderModal({initial: {...emptyModelForm('default'), deployment: 'gpt', displayName: 'gpt'}})
    const details = screen.getByText('Advanced').closest('details')!
    expect(details.open).toBe(false)
    fireEvent.change(contextWindow(), {target: {value: '12.5'}})
    fireEvent.blur(contextWindow())
    expect(contextWindow()).toHaveAccessibleDescription('Enter a whole number')
    fireEvent.change(contextWindow(), {target: {value: '99'}})
    submit()
    expect(details.open).toBe(true)
    expect(contextWindow()).toHaveFocus()
    expect(contextWindow()).toHaveAccessibleDescription('Enter a number from 1,024 to 2,000,000')
  })

  it('refuses a name the resource already has in the same name space', async () => {
    const existing = model('gpt4100000', {deployment: 'gpt-4.1'})
    renderModal({models: [existing]})
    fireEvent.change(deployment(), {target: {value: 'GPT-4.1'}})
    submit()
    expect(deployment()).toHaveFocus()
    expect(deployment()).toHaveAccessibleDescription('This resource already has a model with this name')
    // An embedding of the same name is another name space.
    fireEvent.click(screen.getByRole('radio', {name: 'Embedding'}))
    submit()
    await waitFor(() => expect(api.createAzureModel).toHaveBeenCalledTimes(1))
  })

  it('warns, without blocking, about a display name another model uses', async () => {
    const {onSaved} = renderModal({models: [model('other00000', {displayName: 'Docs writer'})]})
    fireEvent.change(deployment(), {target: {value: 'docs'}})
    fireEvent.change(displayName(), {target: {value: 'docs writer'}})
    expect(displayName()).toHaveAccessibleDescription(/Another model is already called this/)
    expect(displayName()).not.toHaveAttribute('aria-invalid')
    submit()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
  })

  it('refuses a too-long custom purpose on its own field', () => {
    renderModal({initial: {...emptyModelForm('default'), deployment: 'gpt', displayName: 'gpt'}})
    fireEvent.change(screen.getByRole('combobox', {name: 'Purpose'}), {target: {value: 'custom'}})
    const custom = screen.getByRole('textbox', {name: 'Custom purpose'})
    fireEvent.change(custom, {target: {value: 'x'.repeat(33)}})
    submit()
    expect(custom).toHaveFocus()
    expect(custom).toHaveAccessibleDescription('At most 32 characters')
  })
})

describe('AzureModelModal: editing', () => {
  it('sends every editable field, clearing what was emptied', async () => {
    const {onSaved} = renderModal({entry: docs})
    expect(screen.getByRole('dialog', {name: 'Edit Docs writer'})).toBeInTheDocument()
    expect(screen.getByRole('radio', {name: 'Claude'})).toHaveAttribute('aria-checked', 'true')
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {target: {value: ''}})
    fireEvent.change(contextWindow(), {target: {value: ''}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(expect.anything(), false))
    expect(api.updateAzureModel).toHaveBeenCalledWith(USER, 'docs000000', {
      connectionId: 'default',
      kind: 'claude',
      deployment: 'claude-sonnet-4-6',
      displayName: 'Docs writer',
      purpose: 'documentation',
      description: '',
      contextWindow: null,
      maxOutputTokens: null,
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('closes an unchanged edit without a request', async () => {
    const {onClose} = renderModal({entry: docs})
    submit('Save changes')
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(api.updateAzureModel).not.toHaveBeenCalled()
  })

  // U2: PUT /models/{id} is a compare-and-swap on updatedAt. Without it an
  // edit silently overwrote whatever another admin (or tab) saved meanwhile.
  it('saves against the version it loaded: a conflict offers Reload, which moves it to the new version', async () => {
    const shared = {...docs, shared: true, orgId: 'org-1', updatedAt: '2026-09-30T10:00:00.123456Z'}
    const ORG: AzureScope = {kind: 'org', orgId: 'org-1'}
    render(
      <AzureModelModal
        scope={ORG}
        entry={shared}
        initial={modelFormFromEntry(shared)}
        connections={[conn('default', {shared: true})]}
        models={[shared]}
        onClose={vi.fn()}
        onSaved={vi.fn()}
      />,
    )
    api.updateAzureModel.mockRejectedValueOnce(new VersionConflictError('changed meanwhile', 'CONFLICT'))
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {target: {value: 'Mine'}})
    submit('Save changes')
    await waitFor(() => expect(api.updateAzureModel).toHaveBeenCalledTimes(1))
    expect(api.updateAzureModel.mock.calls[0][2]).toMatchObject({
      description: 'Mine',
      updatedAt: '2026-09-30T10:00:00.123456Z',
    })
    const message = await screen.findByText(/Someone else changed this model since you opened it/)
    expect(message.closest('[role="alert"]')).not.toBeNull()

    // Reload shows their version and saves against it from now on.
    api.listAzureConnections.mockResolvedValueOnce({
      connections: [conn('default', {shared: true})],
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    api.listAzureModels.mockResolvedValueOnce({
      models: [{...shared, description: 'Theirs', updatedAt: '2026-09-30T11:00:00.654321Z'}],
    })
    fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
    await waitFor(() => expect(screen.getByRole('textbox', {name: 'Description (optional)'})).toHaveValue('Theirs'))
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {target: {value: 'Mine again'}})
    submit('Save changes')
    await waitFor(() => expect(api.updateAzureModel).toHaveBeenCalledTimes(2))
    expect(api.updateAzureModel.mock.calls[1][2]).toMatchObject({
      description: 'Mine again',
      updatedAt: '2026-09-30T11:00:00.654321Z',
    })
  })

  // Optional (a): the server refuses KIND_REQUIRES_PROJECT only for a change
  // of kind or resource; the client used to block any edit of such an agent.
  it('edits an agent whose resource can no longer run agents, in place', async () => {
    const helper = model('helper0000', {
      kind: 'agent',
      deployment: 'helper',
      displayName: 'Helper',
      status: 'unsupported',
    })
    const {onSaved} = renderModal({entry: helper, connections: [conn('default', {supportsAgents: false})]})
    // The hint stays; the kind is not flagged for an agent that stays where it is.
    expect(screen.getByRole('radiogroup', {name: 'Kind'})).toHaveAccessibleDescription(
      'Agents need a resource with Microsoft Entra ID and a project endpoint.',
    )
    fireEvent.change(screen.getByRole('textbox', {name: 'Description (optional)'}), {target: {value: 'Reviews PRs'}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(api.updateAzureModel.mock.calls[0][2]).toMatchObject({kind: 'agent', description: 'Reviews PRs'})
  })
})

describe('AzureModelModal: server errors', () => {
  it('puts each code on its field and anything else in the form alert', async () => {
    renderModal({initial: {...emptyModelForm('default'), deployment: 'gpt-4.1', displayName: 'gpt-4.1'}})
    await flushFrame()
    api.createAzureModel.mockRejectedValueOnce(
      new VersionConflictError('gpt-4.1 is already on this resource', 'DUPLICATE_MODEL'),
    )
    displayName().focus()
    submit()
    await waitFor(() => expect(deployment()).toHaveAccessibleDescription('gpt-4.1 is already on this resource'))
    expect(deployment()).toHaveFocus()

    api.createAzureModel.mockRejectedValueOnce(new ApiError('window too small', 400, 'INVALID_CONTEXT_WINDOW'))
    submit()
    await waitFor(() => expect(contextWindow()).toHaveAccessibleDescription('window too small'))
    expect(contextWindow()).toHaveFocus()
    expect(screen.getByText('Advanced').closest('details')!.open).toBe(true)

    api.createAzureModel.mockRejectedValueOnce(new VersionConflictError('limit 50', 'TOO_MANY_MODELS'))
    submit()
    const dialog = screen.getByRole('dialog')
    await waitFor(() => expect(within(dialog).getAllByRole('alert')[0]).toHaveTextContent('limit 50'))
  })

  // U4: CONNECTION_NOT_FOUND belongs to the Resource field — which, with a
  // single resource, is plain text: the message used to vanish.
  it('says a vanished resource in the form alert when the Resource is plain text, on its select otherwise', async () => {
    const one = renderModal({initial: {...emptyModelForm('default'), deployment: 'gpt-4.1', displayName: 'gpt-4.1'}})
    await flushFrame()
    api.createAzureModel.mockRejectedValueOnce(new ApiError('the resource was not found', 404, 'CONNECTION_NOT_FOUND'))
    submit()
    const dialog = screen.getByRole('dialog')
    await waitFor(() => expect(within(dialog).getAllByRole('alert')[0]).toHaveTextContent('the resource was not found'))
    expect(one.onSaved).not.toHaveBeenCalled()
    one.unmount()

    renderModal({
      connections: [conn('default'), conn('second0000', {name: 'contoso-prod'})],
      initial: {...emptyModelForm('second0000'), deployment: 'gpt-4.1', displayName: 'gpt-4.1'},
    })
    await flushFrame()
    api.createAzureModel.mockRejectedValueOnce(new ApiError('the resource was not found', 404, 'CONNECTION_NOT_FOUND'))
    submit()
    const select = screen.getByRole('combobox', {name: 'Resource'})
    await waitFor(() => expect(select).toHaveAccessibleDescription('the resource was not found'))
    expect(select).toHaveAttribute('aria-invalid', 'true')
    expect(select).toHaveFocus()
  })

  it('offers Reload after a 409 conflict and shows the saved version', async () => {
    renderModal({entry: docs})
    api.updateAzureModel.mockRejectedValueOnce(new VersionConflictError('changed', 'CONFLICT'))
    fireEvent.change(displayName(), {target: {value: 'Mine'}})
    submit('Save changes')
    const message = await screen.findByText(/Someone else changed this model since you opened it/)
    expect(message.closest('[role="alert"]')).not.toBeNull()

    api.listAzureConnections.mockResolvedValueOnce({
      connections: [conn('default')],
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    api.listAzureModels.mockResolvedValueOnce({models: [{...docs, displayName: 'Their name'}]})
    fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
    await waitFor(() => expect(displayName()).toHaveValue('Their name'))
    expect(screen.queryByText(/Someone else changed this model/)).toBeNull()
  })

  it('confirms a knowledge-base re-index and saves again with force', async () => {
    const kb = model('kb00000000', {kind: 'embedding', deployment: 'embed-3', isKnowledgeEmbedding: true})
    const {onSaved} = renderModal({entry: kb})
    api.updateAzureModel.mockRejectedValueOnce(new VersionConflictError('re-index', 'REINDEX_REQUIRED'))
    fireEvent.change(deployment(), {target: {value: 'embed-4'}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(confirmMock).toHaveBeenCalledWith(expect.objectContaining({title: 'Change the knowledge base model?'}))
    expect(api.updateAzureModel.mock.calls[1][2]).toMatchObject({deployment: 'embed-4', force: true})
  })

  it('explains a list file written by a newer baki in the form alert', async () => {
    renderModal({entry: docs})
    api.updateAzureModel.mockRejectedValueOnce(new VersionConflictError('read-only', 'REGISTRY_READ_ONLY'))
    fireEvent.change(displayName(), {target: {value: 'x'}})
    submit('Save changes')
    expect(await screen.findByText(/Update baki to change these models/)).toBeInTheDocument()
  })

  it('uses permission copy for a 403', async () => {
    renderModal({entry: docs})
    api.updateAzureModel.mockRejectedValueOnce(new ApiError('forbidden', 403, null))
    fireEvent.change(displayName(), {target: {value: 'x'}})
    submit('Save changes')
    expect(
      await screen.findByText('Only organisation admins can change the shared resources and models.'),
    ).toBeInTheDocument()
  })
})

describe('AzureModelModal: closing', () => {
  it('asks before discarding edits, whatever closes it; the backdrop does nothing', async () => {
    const {onClose} = renderModal()
    fireEvent.change(deployment(), {target: {value: 'gpt'}})
    const backdrop = screen.getByRole('dialog').previousElementSibling as HTMLElement
    fireEvent.click(backdrop)
    expect(confirmMock).not.toHaveBeenCalled()

    confirmMock.mockResolvedValueOnce(false)
    fireEvent.click(screen.getByRole('button', {name: 'Close'}))
    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(onClose).not.toHaveBeenCalled()
    expect(deployment()).toHaveValue('gpt')

    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
  })

  it('closes a form pre-filled from discovery without asking while untouched', async () => {
    const {onClose} = renderModal({
      initial: modelFormFromDiscovery(
        {name: 'o4-mini', kind: 'chat', model: 'o4-mini', alreadyAdded: false},
        'default',
      ),
    })
    expect(deployment()).toHaveValue('o4-mini')
    fireEvent.keyDown(document, {key: 'Escape'})
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(confirmMock).not.toHaveBeenCalled()
  })
})

describe('AzureModelModal: the draft test and discovery', () => {
  it('tests the draft {resource, kind, name} and reports the outcome in the right region', async () => {
    renderModal()
    fireEvent.click(screen.getByRole('button', {name: 'Test model'}))
    // Nothing to test without a name.
    expect(api.testAzure).not.toHaveBeenCalled()
    expect(deployment()).toHaveFocus()

    fireEvent.change(deployment(), {target: {value: 'gpt-4.1'}})
    fireEvent.click(screen.getByRole('button', {name: 'Test model'}))
    await waitFor(() => expect(api.testAzure).toHaveBeenCalledTimes(1))
    expect(api.testAzure).toHaveBeenCalledWith(USER, {
      connectionId: 'default',
      model: {kind: 'chat', deployment: 'gpt-4.1'},
    })
    const dialog = screen.getByRole('dialog')
    await waitFor(() => expect(within(dialog).getByRole('status')).toHaveTextContent('Test passed · 40 ms'))

    api.testAzure.mockResolvedValueOnce({ok: false, latencyMs: 0, error: 'deployment not found'})
    fireEvent.click(screen.getByRole('button', {name: 'Test model'}))
    await waitFor(() =>
      expect(
        within(dialog)
          .getAllByRole('alert')
          .some(a => /Test failed deployment not found/.test(a.textContent ?? '')),
      ).toBe(true),
    )
    expect(within(dialog).getByRole('status')).toBeEmptyDOMElement()
    expect(api.createAzureModel).not.toHaveBeenCalled()
  })

  it('offers what discovery found on the resource as chips that fill the name in', async () => {
    api.listAzureConnections.mockResolvedValueOnce({
      connections: [conn('default', {canDiscover: true, supportsAgents: true})],
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    // The registry holds gpt-4.1 already: discovery marks it added.
    api.listAzureModels.mockResolvedValueOnce({models: [model('gpt4100000', {deployment: 'gpt-4.1'})]})
    await useAzureFoundryStore.getState().load(USER)
    api.discoverAzureConnection.mockResolvedValueOnce({
      deployments: [
        {name: 'o4-mini', kind: 'chat', model: 'o4-mini', alreadyAdded: false},
        {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: 'm/x'},
      ],
      agents: [{name: 'Helper', kind: 'agent', model: '', alreadyAdded: false}],
      warnings: [],
    })
    await useAzureFoundryStore.getState().discover(USER, 'default')

    renderModal({connections: [conn('default', {canDiscover: true, supportsAgents: true})]})
    const chips = screen.getByRole('group', {name: 'Found in your project'})
    expect(
      within(chips)
        .getAllByRole('button')
        .map(b => b.textContent),
    ).toEqual(['o4-mini'])
    fireEvent.click(within(chips).getByRole('button', {name: 'Use o4-mini'}))
    expect(deployment()).toHaveValue('o4-mini')
    expect(displayName()).toHaveValue('o4-mini')
    // Agents are offered for an agent.
    fireEvent.click(screen.getByRole('radio', {name: 'Agent'}))
    expect(
      within(screen.getByRole('group', {name: 'Found in your project'})).getByRole('button', {name: 'Use Helper'}),
    ).toBeInTheDocument()
  })
})
