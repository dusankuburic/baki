import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import AzureConnectionModal, {type ConnectionModalMode} from './AzureConnectionModal'
import {providersApi} from '@/api'
import {ApiError, VersionConflictError} from '@/api/client'
import {resetAllStores} from '@/stores/storeRegistry'
import type {AzureConnection, AzureScope} from '@/types'

// The discard and re-index confirms go through the shared useConfirm hook:
// a partial mock of the real barrel keeps every other primitive real.
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
      testAzure: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)
const USER: AzureScope = {kind: 'user'}
const ORG: AzureScope = {kind: 'org', orgId: 'org-1'}

// Real GUID shapes: the form rejects Entra IDs that are not GUIDs.
const TENANT_GUID = '11111111-2222-4333-8444-555555555555'
const CLIENT_GUID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee'
const KEEP = 'Leave blank to keep the stored secret'
const REENTER = /re-enter it for this change/

function conn(over: Partial<AzureConnection> = {}): AzureConnection {
  return {
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
    ...over,
  }
}

const entra = (over: Partial<AzureConnection> = {}) =>
  conn({
    config: {
      endpoint: 'https://my-res.services.ai.azure.com',
      authMode: 'entra',
      hasSecret: true,
      tenantId: TENANT_GUID,
      clientId: CLIENT_GUID,
      projectEndpoint: 'https://my-res.services.ai.azure.com/api/projects/my-project',
    },
    canDiscover: true,
    supportsAgents: true,
    ...over,
  })

function renderModal(
  opts: {
    mode?: ConnectionModalMode
    connection?: AzureConnection | null
    allowCli?: boolean
    scope?: AzureScope
  } = {},
) {
  const onClose = vi.fn()
  const onSaved = vi.fn()
  const utils = render(
    <AzureConnectionModal
      scope={opts.scope ?? USER}
      mode={opts.mode ?? (opts.connection ? 'edit' : 'add')}
      connection={opts.connection ?? null}
      allowCli={opts.allowCli ?? false}
      onClose={onClose}
      onSaved={onSaved}
    />,
  )
  return {...utils, onClose, onSaved}
}

// Modal focuses inside a rAF.
async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

const endpoint = () => screen.getByRole('textbox', {name: 'Endpoint'})
const nameField = () => screen.getByRole('textbox', {name: 'Name'})
const apiKey = () => screen.getByLabelText('API key', {selector: 'input'})
const clientSecret = () => screen.getByLabelText('Client secret', {selector: 'input'})
const project = () => screen.getByRole('textbox', {name: 'Project endpoint (optional)'})
const submit = (name = 'Add resource') => fireEvent.click(screen.getByRole('button', {name}))

beforeEach(async () => {
  vi.resetAllMocks()
  await resetAllStores()
  confirmMock.mockResolvedValue(true)
  api.createAzureConnection.mockImplementation(async (_scope, input) =>
    conn({id: 'newres0000', name: input.name, legacy: false}),
  )
  api.updateAzureConnection.mockImplementation(async (_scope, id, input) => conn({id, name: input.name ?? 'my-res'}))
  api.testAzure.mockResolvedValue({ok: true, latencyMs: 12})
})

describe('AzureConnectionModal: the form', () => {
  it('opens as a named dialog on the endpoint, with API-key sign-in and a guarded secret field', async () => {
    renderModal()
    const dialog = screen.getByRole('dialog', {name: 'Add resource'})
    expect(dialog).toHaveAccessibleDescription(/its endpoint and how baki signs in/)
    await flushFrame()
    expect(endpoint()).toHaveFocus()

    expect(screen.getByRole('radio', {name: 'API key'})).toHaveAttribute('aria-checked', 'true')
    const secret = apiKey()
    expect(secret).toHaveAttribute('type', 'password')
    // "off" is ignored on password fields by password managers.
    expect(secret).toHaveAttribute('autocomplete', 'new-password')
    expect(secret).toHaveAttribute('spellcheck', 'false')
    expect(secret).not.toHaveAttribute('placeholder', KEEP)

    // A toggle whose name says which secret it reveals.
    const show = screen.getByRole('button', {name: 'Show API key'})
    expect(show).toHaveAttribute('aria-pressed', 'false')
    fireEvent.click(show)
    expect(apiKey()).toHaveAttribute('type', 'text')
    fireEvent.click(screen.getByRole('button', {name: 'Hide API key'}))
    expect(apiKey()).toHaveAttribute('type', 'password')

    // An API key has no project: an info line stands in for the field.
    expect(screen.queryByRole('textbox', {name: 'Project endpoint (optional)'})).toBeNull()
    expect(screen.getByText(/An API key can't list deployments: add them by name/)).toBeInTheDocument()
  })

  it('goes full screen below 640 px and submits through form= from the footer', () => {
    renderModal()
    const dialog = screen.getByRole('dialog')
    expect(dialog.className).toContain('max-sm:h-full')
    const save = screen.getByRole('button', {name: 'Add resource'})
    const form = dialog.querySelector('form')!
    expect(form).toHaveAttribute('novalidate')
    expect(save).toHaveAttribute('type', 'submit')
    expect(save).toHaveAttribute('form', form.id)
    expect(form.contains(save)).toBe(false)
    // Save stays enabled on an invalid form: submitting says what is wrong.
    expect(save).toBeEnabled()
  })

  it('names the resource after its endpoint host until the name is edited', () => {
    renderModal()
    fireEvent.change(endpoint(), {target: {value: 'https://contoso-prod.openai.azure.com'}})
    expect(nameField()).toHaveValue('contoso-prod')
    fireEvent.change(nameField(), {target: {value: 'Production'}})
    fireEvent.change(endpoint(), {target: {value: 'https://other.openai.azure.com'}})
    expect(nameField()).toHaveValue('Production')
  })

  it('swaps the credential fields with the sign-in mode', () => {
    renderModal()
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    expect(screen.getByRole('textbox', {name: 'Tenant ID'})).toBeInTheDocument()
    expect(screen.getByRole('textbox', {name: 'Client ID'})).toBeInTheDocument()
    expect(clientSecret()).toHaveAttribute('type', 'password')
    expect(project()).toBeInTheDocument()
    expect(screen.queryByLabelText('API key', {selector: 'input'})).toBeNull()

    fireEvent.click(screen.getByRole('radio', {name: 'API key'}))
    expect(apiKey()).toBeInTheDocument()
    expect(screen.queryByRole('textbox', {name: 'Tenant ID'})).toBeNull()
  })

  // The Azure CLI's signed-in user is the credential — desktop only,
  // never for an organisation's resource (the host passes allowCli).
  it('offers the Azure CLI only where allowed, with no secret and an optional tenant', async () => {
    const {unmount} = renderModal()
    expect(screen.queryByRole('radio', {name: 'Azure CLI'})).toBeNull()
    expect(screen.getByRole('radio', {name: 'Microsoft Entra ID'})).toBeInTheDocument()
    unmount()

    const {onSaved} = renderModal({allowCli: true})
    fireEvent.click(screen.getByRole('radio', {name: 'Azure CLI'}))
    expect(screen.getByRole('textbox', {name: 'Tenant ID'})).toHaveAccessibleDescription(
      /Leave blank to use the Azure CLI signed-in tenant/,
    )
    expect(screen.queryByRole('textbox', {name: 'Client ID'})).toBeNull()
    expect(screen.queryByLabelText('Client secret', {selector: 'input'})).toBeNull()
    expect(screen.queryByLabelText('API key', {selector: 'input'})).toBeNull()

    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    submit()
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1))
    expect(api.createAzureConnection).toHaveBeenCalledWith(USER, {
      name: 'my-res',
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'entra_cli', tenantId: TENANT_GUID}},
    })
  })

  it('keeps a stored Azure CLI sign-in visible, but refuses it where it is not allowed', async () => {
    const cli = conn({config: {endpoint: 'https://my-res.openai.azure.com', authMode: 'entra_cli', hasSecret: false}})
    renderModal({connection: cli, scope: ORG})
    const radio = screen.getByRole('radio', {name: 'Azure CLI'})
    expect(radio).toHaveAttribute('aria-checked', 'true')
    expect(radio).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByRole('radiogroup', {name: 'Authentication'})).toHaveAccessibleDescription(
      /works only in the desktop app, and not for an organisation's resource/,
    )
    submit('Save changes')
    expect(api.updateAzureConnection).not.toHaveBeenCalled()
  })
})

describe('AzureConnectionModal: validation and focus', () => {
  it('validates on submit, focusing the first invalid field; a blank field is not flagged while typing', async () => {
    renderModal()
    fireEvent.blur(endpoint())
    expect(screen.queryByText('Required')).toBeNull()

    submit()
    expect(endpoint()).toHaveFocus()
    expect(endpoint()).toHaveAttribute('aria-invalid', 'true')
    expect(endpoint()).toHaveAccessibleDescription('Required')
    expect(api.createAzureConnection).not.toHaveBeenCalled()

    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    submit()
    // The name followed the endpoint; the secret is what is missing now.
    expect(apiKey()).toHaveFocus()
    expect(apiKey()).toHaveAttribute('aria-invalid', 'true')

    fireEvent.change(apiKey(), {target: {value: 'sk-test'}})
    submit()
    await waitFor(() => expect(api.createAzureConnection).toHaveBeenCalledTimes(1))
    expect(api.createAzureConnection.mock.calls[0]![1]).toStrictEqual({
      name: 'my-res',
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-test'}},
    })
  })

  it('checks the endpoint format once the field is left', () => {
    renderModal()
    fireEvent.change(endpoint(), {target: {value: 'http://my-res.openai.azure.com'}})
    expect(screen.queryByText('The endpoint must use https://')).toBeNull()
    fireEvent.blur(endpoint())
    expect(endpoint()).toHaveAccessibleDescription('The endpoint must use https://')

    fireEvent.change(endpoint(), {target: {value: 'my-res.openai.azure.com'}})
    expect(endpoint()).toHaveAccessibleDescription('The endpoint must be an absolute https:// URL')
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    expect(endpoint()).not.toHaveAttribute('aria-invalid')
  })

  it('flags a non-Azure host as advice only and lets the server rule on it', async () => {
    const {onSaved} = renderModal()
    fireEvent.change(endpoint(), {target: {value: 'https://ai.contoso.example'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-test'}})
    fireEvent.blur(endpoint())
    expect(endpoint()).toHaveAccessibleDescription(/not an Azure AI Foundry endpoint/)
    expect(endpoint()).not.toHaveAttribute('aria-invalid')
    submit()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
  })

  it('accepts a tenant domain, and requires GUID-shaped Entra IDs', async () => {
    renderModal()
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    const tenant = screen.getByRole('textbox', {name: 'Tenant ID'})
    const client = screen.getByRole('textbox', {name: 'Client ID'})
    fireEvent.change(tenant, {target: {value: 'contoso.onmicrosoft.com'}})
    fireEvent.blur(tenant)
    expect(tenant).not.toHaveAttribute('aria-invalid')
    fireEvent.change(tenant, {target: {value: 'common'}})
    expect(tenant).toHaveAccessibleDescription(/directory \(tenant\) ID GUID/)

    fireEvent.change(tenant, {target: {value: TENANT_GUID}})
    fireEvent.change(client, {target: {value: 'client-guid'}})
    fireEvent.change(clientSecret(), {target: {value: 's3cret'}})
    submit()
    expect(client).toHaveFocus()
    expect(client).toHaveAccessibleDescription(/Must be a GUID/)
    expect(api.createAzureConnection).not.toHaveBeenCalled()

    fireEvent.change(client, {target: {value: CLIENT_GUID}})
    fireEvent.change(project(), {target: {value: 'https://my-res.services.ai.azure.com/api/projects/p'}})
    submit()
    await waitFor(() => expect(api.createAzureConnection).toHaveBeenCalledTimes(1))
    expect(api.createAzureConnection.mock.calls[0]![1]).toStrictEqual({
      name: 'my-res',
      config: {
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'entra', tenantId: TENANT_GUID, clientId: CLIENT_GUID, clientSecret: 's3cret'},
        projectEndpoint: 'https://my-res.services.ai.azure.com/api/projects/p',
      },
    })
  })
})

// The placeholder promises the stored secret only while the server
// would carry it — another host, project host or Entra principal needs the
// secret again, and the form says so before the round trip.
describe('AzureConnectionModal: carrying the stored secret', () => {
  it('keeps the secret for an unchanged edit and sends no secret', async () => {
    const {onSaved} = renderModal({connection: conn()})
    expect(apiKey()).toHaveValue('')
    expect(apiKey()).toHaveAttribute('placeholder', KEEP)
    fireEvent.change(nameField(), {target: {value: 'renamed'}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(expect.objectContaining({name: 'renamed'}), false))
    expect(api.updateAzureConnection).toHaveBeenCalledWith(USER, 'default', {
      name: 'renamed',
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('drops the carry when the endpoint moves to another resource, and asks for the secret', async () => {
    renderModal({connection: conn({modelCount: 3})})
    fireEvent.change(endpoint(), {target: {value: 'https://other-res.openai.azure.com'}})
    expect(apiKey()).not.toHaveAttribute('placeholder', KEEP)
    expect(apiKey()).toHaveAccessibleDescription(REENTER)
    // The models stay with the resource.
    expect(screen.getByText('The resource keeps its 3 models; test them after saving.')).toBeInTheDocument()

    submit('Save changes')
    expect(apiKey()).toHaveFocus()
    expect(apiKey()).toHaveAttribute('aria-invalid', 'true')
    expect(apiKey()).toHaveAccessibleDescription(REENTER)
    expect(api.updateAzureConnection).not.toHaveBeenCalled()

    fireEvent.change(apiKey(), {target: {value: 'sk-new'}})
    submit('Save changes')
    await waitFor(() => expect(api.updateAzureConnection).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection.mock.calls[0]![2]).toStrictEqual({
      config: {endpoint: 'https://other-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-new'}},
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('keeps the carry for a project endpoint on the same host, not on another', () => {
    renderModal({connection: entra()})
    expect(clientSecret()).toHaveAttribute('placeholder', KEEP)
    fireEvent.change(project(), {target: {value: 'https://other-res.services.ai.azure.com/api/projects/p1'}})
    expect(clientSecret()).not.toHaveAttribute('placeholder', KEEP)
    fireEvent.change(project(), {target: {value: 'https://my-res.services.ai.azure.com/api/projects/p1'}})
    expect(clientSecret()).toHaveAttribute('placeholder', KEEP)
  })

  it('drops the carry for another Entra principal, keeps it for the same one in another case', () => {
    renderModal({connection: entra()})
    const client = screen.getByRole('textbox', {name: 'Client ID'})
    fireEvent.change(client, {target: {value: 'ffffffff-ffff-4fff-8fff-ffffffffffff'}})
    expect(clientSecret()).toHaveAccessibleDescription(REENTER)
    fireEvent.change(client, {target: {value: CLIENT_GUID.toUpperCase()}})
    expect(clientSecret()).toHaveAttribute('placeholder', KEEP)
  })

  it('needs a new secret when the sign-in mode changes', () => {
    renderModal({connection: conn()})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    // The stored API key cannot stand in for a client secret.
    expect(clientSecret()).not.toHaveAttribute('placeholder', KEEP)
  })

  it('asks for the credentials again for a resource that needs them, focusing the secret', async () => {
    renderModal({mode: 'reauth', connection: conn({status: 'needs_credentials', statusReason: 'Lender left'})})
    expect(screen.getByRole('dialog', {name: 'Enter credentials for my-res'})).toBeInTheDocument()
    await flushFrame()
    expect(apiKey()).toHaveFocus()
    // Nothing usable to carry.
    expect(apiKey()).not.toHaveAttribute('placeholder', KEEP)
    submit('Save credentials')
    expect(apiKey()).toHaveAttribute('aria-invalid', 'true')
    fireEvent.change(apiKey(), {target: {value: 'sk-again'}})
    submit('Save credentials')
    await waitFor(() => expect(api.updateAzureConnection).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection.mock.calls[0]![2]).toMatchObject({
      config: {auth: {mode: 'api_key', key: 'sk-again'}},
    })
  })

  // U1: the server sends blank settings ({endpoint: '', authMode: '',
  // hasSecret: false}) when the stored credential document is gone (a
  // lender's credential disabled) or unreadable. The form used to select no
  // sign-in mode while it showed the Entra ID fields, read their secret as
  // blank ("Required"), and focus an API-key field that wasn't there: Save
  // sent nothing.
  it('enters credentials for a resource whose settings came back blank', async () => {
    const blank = conn({
      status: 'unreadable',
      config: {endpoint: '', authMode: '', hasSecret: false},
      modelCount: 2,
    })
    const {onSaved} = renderModal({mode: 'reauth', connection: blank})
    expect(screen.getByRole('dialog', {name: 'Enter credentials for my-res'})).toBeInTheDocument()
    // One sign-in mode is selected, and its fields are the ones shown.
    expect(screen.getByRole('radio', {name: 'API key'})).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('radio', {name: 'Microsoft Entra ID'})).toHaveAttribute('aria-checked', 'false')
    expect(screen.queryByRole('textbox', {name: 'Tenant ID'})).toBeNull()
    // Nothing to read the secret against yet: the endpoint comes first.
    await flushFrame()
    expect(endpoint()).toHaveFocus()

    submit('Save credentials')
    expect(endpoint()).toHaveFocus()
    expect(endpoint()).toHaveAccessibleDescription('Required')
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    submit('Save credentials')
    expect(apiKey()).toHaveFocus()
    expect(apiKey()).toHaveAttribute('aria-invalid', 'true')
    fireEvent.change(apiKey(), {target: {value: 'sk-again'}})
    submit('Save credentials')
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection.mock.calls[0]![2]).toStrictEqual({
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-again'}},
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('enters Entra ID credentials for a resource whose settings came back blank', async () => {
    const blank = conn({status: 'needs_credentials', config: {endpoint: '', authMode: '', hasSecret: false}})
    const {onSaved} = renderModal({mode: 'reauth', connection: blank})
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})
    submit('Save credentials')
    // The secret asked for is the one on screen.
    expect(clientSecret()).toHaveFocus()
    expect(clientSecret()).toHaveAttribute('aria-invalid', 'true')
    fireEvent.change(clientSecret(), {target: {value: 's3cret'}})
    fireEvent.change(project(), {target: {value: 'https://my-res.services.ai.azure.com/api/projects/p'}})
    submit('Save credentials')
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection.mock.calls[0]![2]).toStrictEqual({
      config: {
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'entra', tenantId: TENANT_GUID, clientId: CLIENT_GUID, clientSecret: 's3cret'},
        projectEndpoint: 'https://my-res.services.ai.azure.com/api/projects/p',
      },
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('renames a resource that needs credentials without asking for its secret', async () => {
    const {onSaved} = renderModal({connection: conn({status: 'needs_credentials'})})
    fireEvent.change(nameField(), {target: {value: 'renamed'}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(api.updateAzureConnection.mock.calls[0]![2]).toStrictEqual({
      name: 'renamed',
      updatedAt: '2026-09-30T10:00:00Z',
    })
  })

  it('closes an edit without a request when nothing changed', async () => {
    const {onClose} = renderModal({connection: conn()})
    submit('Save changes')
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection).not.toHaveBeenCalled()
  })
})

describe('AzureConnectionModal: server errors', () => {
  async function fillAndSave() {
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-test'}})
    // Focus away from the field the server blames, so moving it is visible.
    apiKey().focus()
    submit()
  }

  it('puts each code on its field, and anything else in the form alert', async () => {
    renderModal()
    // Let the dialog's own initial focus land first.
    await flushFrame()
    api.createAzureConnection.mockRejectedValueOnce(new ApiError('host rejected', 400, 'HOST_NOT_ALLOWED'))
    await fillAndSave()
    // The allow-list verdict gets the local copy naming the accepted hosts.
    await waitFor(() => expect(endpoint()).toHaveAccessibleDescription(/not an Azure AI Foundry endpoint/))
    expect(endpoint()).toHaveAttribute('aria-invalid', 'true')
    expect(endpoint()).toHaveFocus()

    api.createAzureConnection.mockRejectedValueOnce(
      new ApiError('a secret for this endpoint must be entered', 400, 'MISSING_SECRET'),
    )
    submit()
    await waitFor(() => expect(apiKey()).toHaveAccessibleDescription('a secret for this endpoint must be entered'))
    expect(apiKey()).toHaveFocus()
    // Editing the field clears the server's verdict.
    fireEvent.change(apiKey(), {target: {value: 'sk-test2'}})
    expect(apiKey()).not.toHaveAttribute('aria-invalid')

    api.createAzureConnection.mockRejectedValueOnce(
      new VersionConflictError('a resource called my-res exists', 'DUPLICATE_CONNECTION_NAME'),
    )
    submit()
    await waitFor(() => expect(nameField()).toHaveAccessibleDescription('a resource called my-res exists'))

    api.createAzureConnection.mockRejectedValueOnce(new VersionConflictError('limit 10', 'TOO_MANY_CONNECTIONS'))
    submit()
    const dialog = screen.getByRole('dialog')
    await waitFor(() => expect(within(dialog).getAllByRole('alert')[0]).toHaveTextContent('limit 10'))
    // The form and its values survive rejected saves.
    expect(endpoint()).toHaveValue('https://my-res.services.ai.azure.com')
  })

  it('puts a rejected project endpoint on its field with the server text', async () => {
    renderModal()
    await flushFrame()
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})
    fireEvent.change(clientSecret(), {target: {value: 's3cret'}})
    fireEvent.change(project(), {target: {value: 'https://elsewhere.example.com/api/projects/p'}})
    api.createAzureConnection.mockRejectedValueOnce(
      new ApiError('project endpoint host "elsewhere.example.com" is not allowed', 400, 'INVALID_PROJECT_ENDPOINT'),
    )
    submit()
    await waitFor(() =>
      expect(project()).toHaveAccessibleDescription('project endpoint host "elsewhere.example.com" is not allowed'),
    )
    expect(project()).toHaveFocus()
  })

  it("explains a concurrent change (409) and reloads the other admin's version on request", async () => {
    const shared = conn({shared: true, credentialUpdatedAt: '2026-09-30T09:00:00.123456Z'})
    renderModal({connection: shared, scope: ORG})
    api.updateAzureConnection.mockRejectedValueOnce(new VersionConflictError('changed', 'CONFLICT'))
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com/'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-mine'}})
    submit('Save changes')
    // The credential's own stamp went along (org compare-and-swap).
    await waitFor(() => expect(api.updateAzureConnection).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection.mock.calls[0]![2]).toMatchObject({
      credentialUpdatedAt: '2026-09-30T09:00:00.123456Z',
      updatedAt: '2026-09-30T10:00:00Z',
    })
    const message = await screen.findByText(/Someone else changed this resource since you opened it/)
    expect(message.closest('[role="alert"]')).not.toBeNull()

    api.listAzureConnections.mockResolvedValueOnce({
      connections: [
        {...shared, name: 'renamed-by-bo', config: {...shared.config, endpoint: 'https://bo.openai.azure.com'}},
      ],
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    api.listAzureModels.mockResolvedValueOnce({models: []})
    fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
    await waitFor(() => expect(endpoint()).toHaveValue('https://bo.openai.azure.com'))
    expect(nameField()).toHaveValue('renamed-by-bo')
    expect(screen.queryByText(/Someone else changed this resource/)).toBeNull()
    expect(api.listAzureConnections).toHaveBeenCalledWith(ORG)
  })

  it('puts a credential refusal on the field the server names', async () => {
    renderModal()
    await flushFrame()
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    const tenant = screen.getByRole('textbox', {name: 'Tenant ID'})
    fireEvent.change(tenant, {target: {value: TENANT_GUID}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})
    fireEvent.change(clientSecret(), {target: {value: 's3cret'}})
    api.createAzureConnection.mockRejectedValueOnce(
      new ApiError('this directory does not exist', 400, 'INVALID_AUTH', undefined, {field: 'tenantId'}),
    )
    submit()
    await waitFor(() => expect(tenant).toHaveAccessibleDescription('this directory does not exist'))
    expect(tenant).toHaveFocus()
  })

  it('puts a refusal of a field that is not on screen in the form alert', async () => {
    renderModal()
    await flushFrame()
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-test'}})
    // The tenant field belongs to Entra ID sign-in, not shown for an API key.
    api.createAzureConnection.mockRejectedValueOnce(
      new ApiError('this directory does not exist', 400, 'INVALID_AUTH', undefined, {field: 'tenantId'}),
    )
    submit()
    const dialog = screen.getByRole('dialog')
    await waitFor(() =>
      expect(within(dialog).getAllByRole('alert')[0]).toHaveTextContent('this directory does not exist'),
    )
  })

  it('explains a list file written by a newer baki in the form alert', async () => {
    renderModal({connection: conn()})
    api.updateAzureConnection.mockRejectedValueOnce(
      new VersionConflictError('the model list is read-only', 'REGISTRY_READ_ONLY'),
    )
    fireEvent.change(nameField(), {target: {value: 'x'}})
    submit('Save changes')
    expect(await screen.findByText(/Update baki to change these models/)).toBeInTheDocument()
    expect(screen.queryByText('the model list is read-only')).toBeNull()
  })

  it('uses permission copy for a 403', async () => {
    renderModal({connection: conn({shared: true}), scope: ORG})
    api.updateAzureConnection.mockRejectedValueOnce(new ApiError('forbidden', 403, null))
    fireEvent.change(nameField(), {target: {value: 'x'}})
    submit('Save changes')
    expect(
      await screen.findByText('Only organisation admins can change the shared resources and models.'),
    ).toBeInTheDocument()
    expect(screen.queryByText('forbidden')).toBeNull()
  })

  it('asks before an endpoint change that makes the knowledge base re-index, then confirms it with force', async () => {
    const {onSaved} = renderModal({connection: conn({modelCount: 1})})
    api.updateAzureConnection.mockRejectedValueOnce(new VersionConflictError('re-index', 'REINDEX_REQUIRED'))
    fireEvent.change(endpoint(), {target: {value: 'https://new-res.openai.azure.com'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-new'}})
    submit('Save changes')
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(confirmMock).toHaveBeenCalledWith(expect.objectContaining({title: 'Change the knowledge base model?'}))
    expect(api.updateAzureConnection).toHaveBeenCalledTimes(2)
    expect(api.updateAzureConnection.mock.calls[1]![2]).toMatchObject({force: true})
  })

  it('leaves it at the confirm when the re-index is declined', async () => {
    const {onSaved} = renderModal({connection: conn({modelCount: 1})})
    confirmMock.mockResolvedValueOnce(false)
    api.updateAzureConnection.mockRejectedValueOnce(new VersionConflictError('re-index', 'REINDEX_REQUIRED'))
    fireEvent.change(nameField(), {target: {value: 'renamed'}})
    submit('Save changes')
    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(api.updateAzureConnection).toHaveBeenCalledTimes(1)
    expect(onSaved).not.toHaveBeenCalled()
  })
})

describe('AzureConnectionModal: closing', () => {
  it('closes a pristine form at once, and a dirty one only after the discard confirm', async () => {
    const pristine = renderModal()
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(pristine.onClose).toHaveBeenCalledTimes(1))
    expect(confirmMock).not.toHaveBeenCalled()
    pristine.unmount()

    const dirty = renderModal()
    fireEvent.change(endpoint(), {target: {value: 'https://x.openai.azure.com'}})
    confirmMock.mockResolvedValueOnce(false)
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(confirmMock).toHaveBeenCalledWith(
      expect.objectContaining({title: 'Discard unsaved changes?', confirmLabel: 'Discard', danger: true}),
    )
    // The edits survive a rejected discard.
    expect(dirty.onClose).not.toHaveBeenCalled()
    expect(endpoint()).toHaveValue('https://x.openai.azure.com')

    // Escape goes the same way; the X too.
    fireEvent.keyDown(document, {key: 'Escape'})
    await waitFor(() => expect(dirty.onClose).toHaveBeenCalledTimes(1))
    expect(confirmMock).toHaveBeenCalledTimes(2)
  })

  it('ignores a click on the backdrop', () => {
    const {onClose} = renderModal()
    fireEvent.change(endpoint(), {target: {value: 'https://x.openai.azure.com'}})
    const backdrop = screen.getByRole('dialog').previousElementSibling as HTMLElement
    fireEvent.click(backdrop)
    expect(onClose).not.toHaveBeenCalled()
    expect(confirmMock).not.toHaveBeenCalled()
  })
})

describe('AzureConnectionModal: testing the draft', () => {
  it('tests the draft settings without saving, and says it costs nothing', async () => {
    renderModal({connection: conn()})
    const test = screen.getByRole('button', {name: 'Test resource'})
    expect(test).toHaveAccessibleDescription('Checks sign-in and the endpoint only: no model call, no cost.')
    fireEvent.click(test)
    await waitFor(() => expect(api.testAzure).toHaveBeenCalledTimes(1))
    // The saved resource's id lets the stored secret carry over.
    expect(api.testAzure).toHaveBeenCalledWith(USER, {
      connectionId: 'default',
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key'}},
    })
    const dialog = screen.getByRole('dialog')
    // A pass is read out politely; the alert region stays empty.
    await waitFor(() => expect(within(dialog).getByRole('status')).toHaveTextContent('Test passed · 12 ms'))
    expect(api.updateAzureConnection).not.toHaveBeenCalled()
  })

  it('reports a failure as one alert with the reason', async () => {
    renderModal()
    api.testAzure.mockResolvedValueOnce({ok: false, latencyMs: 0, error: 'the key was refused (401)'})
    fireEvent.change(endpoint(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(apiKey(), {target: {value: 'sk-bad'}})
    fireEvent.click(screen.getByRole('button', {name: 'Test resource'}))
    const dialog = screen.getByRole('dialog')
    await waitFor(() => {
      const alert = within(dialog)
        .getAllByRole('alert')
        .find(a => a.textContent?.includes('Test failed'))
      expect(alert).toHaveTextContent('Test failed the key was refused (401)')
    })
    expect(within(dialog).getByRole('status')).toBeEmptyDOMElement()
    // A new draft draws the test again.
    expect(api.testAzure).toHaveBeenCalledWith(USER, {
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-bad'}},
    })
  })

  it('does not test a draft that is not valid yet', () => {
    renderModal()
    fireEvent.click(screen.getByRole('button', {name: 'Test resource'}))
    expect(api.testAzure).not.toHaveBeenCalled()
    expect(endpoint()).toHaveFocus()
  })
})
