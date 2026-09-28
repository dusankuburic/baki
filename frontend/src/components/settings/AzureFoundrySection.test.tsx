import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import AzureFoundrySection from './AzureFoundrySection'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import {useSystemStore} from '@/stores/systemStore'
import type {AppInfo, AzureFoundryPublicConfig, ProviderTestResult} from '@/types'

// The section's confirm dialogs (remove / discard-unsaved) go through the
// shared useConfirm hook; a controllable mock keeps the tests on the section's
// own rendering instead of the dialog chrome.
const confirmMock = vi.hoisted(() => vi.fn())
vi.mock('@/components/shared', () => ({
  useConfirm: () => ({confirm: confirmMock}),
}))

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      getAzureFoundryConfig: vi.fn(),
      saveAzureFoundryConfig: vi.fn(),
      deleteApiKey: vi.fn(),
      testProviderConnection: vi.fn(),
      getOrgAzureFoundryConfig: vi.fn(),
      saveOrgAzureFoundryConfig: vi.fn(),
      deleteOrgAzureFoundryConfig: vi.fn(),
      testOrgAzureFoundryConnection: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)

const info = (sessionAnalytics: boolean): AppInfo => ({
  version: 'dev',
  platform: 'linux',
  arch: 'amd64',
  buildDate: '',
  gitCommit: '',
  capabilities: {sessionAnalytics},
})

// Real GUID shapes: the form now rejects Entra IDs that are not GUIDs.
const TENANT_GUID = '11111111-2222-4333-8444-555555555555'
const CLIENT_GUID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee'

const apiKeyConfig: AzureFoundryPublicConfig = {
  configured: true,
  endpoint: 'https://my-res.openai.azure.com',
  authMode: 'api_key',
  hasSecret: true,
  chatDeployment: 'gpt-4.1',
}

const entraConfig: AzureFoundryPublicConfig = {
  configured: true,
  endpoint: 'https://my-res.services.ai.azure.com',
  authMode: 'entra',
  hasSecret: true,
  tenantId: 'tenant-guid',
  clientId: 'client-guid',
  projectEndpoint: 'https://my-res.services.ai.azure.com/api/projects/my-project',
  embeddingDeployment: 'text-embedding-3-small',
}

const endpointField = () => screen.getByRole('textbox', {name: 'Azure endpoint'})
const saveButton = () => screen.getByRole('button', {name: 'Save'})

async function renderUnconfigured(onConfigured?: () => void) {
  render(<AzureFoundrySection onConfigured={onConfigured} />)
  await screen.findByRole('textbox', {name: 'Azure endpoint'})
}

beforeEach(() => {
  vi.resetAllMocks()
  confirmMock.mockResolvedValue(true)
  useSystemStore.setState({info: info(false), isLoaded: true})
  api.getAzureFoundryConfig.mockResolvedValue({configured: false})
  api.saveAzureFoundryConfig.mockResolvedValue(undefined)
  api.deleteApiKey.mockResolvedValue(undefined)
  api.testProviderConnection.mockResolvedValue({ok: true, latencyMs: 12})
  api.getOrgAzureFoundryConfig.mockResolvedValue({configured: false})
  api.saveOrgAzureFoundryConfig.mockResolvedValue(undefined)
  api.deleteOrgAzureFoundryConfig.mockResolvedValue(undefined)
  api.testOrgAzureFoundryConnection.mockResolvedValue({ok: true, latencyMs: 34})
})

describe('AzureFoundrySection', () => {
  it('shows the empty form with API-key auth when nothing is configured', async () => {
    await renderUnconfigured()
    expect(screen.getByRole('radio', {name: 'API key'})).toHaveAttribute('aria-checked', 'true')
    const secret = screen.getByLabelText('Azure API key')
    expect(secret).toHaveAttribute('type', 'password')
    // "off" is ignored on password fields by password managers.
    expect(secret).toHaveAttribute('autocomplete', 'new-password')
    expect(secret).toHaveAttribute('spellcheck', 'false')
    expect(screen.queryByRole('textbox', {name: 'Tenant ID'})).toBeNull()
    expect(saveButton()).toBeDisabled()
    expect(screen.queryByRole('button', {name: 'Cancel'})).toBeNull()

    // A toggle whose name says which secret it reveals.
    const show = screen.getByRole('button', {name: 'Show API key'})
    expect(show).toHaveAttribute('aria-pressed', 'false')
    fireEvent.click(show)
    expect(screen.getByLabelText('Azure API key')).toHaveAttribute('type', 'text')
    expect(screen.getByRole('button', {name: 'Hide API key'})).toHaveAttribute('aria-pressed', 'true')
    fireEvent.click(screen.getByRole('button', {name: 'Hide API key'}))
    expect(screen.getByLabelText('Azure API key')).toHaveAttribute('type', 'password')
  })

  it('swaps the credential fields with the auth mode', async () => {
    await renderUnconfigured()
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    expect(screen.getByRole('textbox', {name: 'Tenant ID'})).toBeInTheDocument()
    expect(screen.getByRole('textbox', {name: 'Client ID'})).toBeInTheDocument()
    expect(screen.getByLabelText('Azure client secret')).toHaveAttribute('type', 'password')
    expect(screen.queryByLabelText('Azure API key')).toBeNull()

    fireEvent.click(screen.getByRole('radio', {name: 'API key'}))
    expect(screen.getByLabelText('Azure API key')).toBeInTheDocument()
    expect(screen.queryByRole('textbox', {name: 'Tenant ID'})).toBeNull()
  })

  it('posts the normalised body, reloads the view and reports the change', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValueOnce({configured: false}).mockResolvedValue(apiKeyConfig)
    await renderUnconfigured(onConfigured)

    fireEvent.change(endpointField(), {target: {value: '  https://my-res.openai.azure.com '}})
    // A secret is still missing.
    expect(saveButton()).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
      target: {value: 'gpt-4.1'},
    })
    expect(saveButton()).toBeEnabled()
    fireEvent.click(saveButton())

    await waitFor(() => expect(onConfigured).toHaveBeenCalledTimes(1))
    expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1)
    expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
      endpoint: 'https://my-res.openai.azure.com',
      auth: {mode: 'api_key', key: 'sk-test'},
      chatDeployment: 'gpt-4.1',
    })
    // The summary comes from the fresh GET (the micro-cache is bypassed), not
    // from the form.
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(2)
    expect(screen.getByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.getByText('gpt-4.1')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Edit'})).toBeInTheDocument()
  })

  it('holds a skeleton while the post-save reload is in flight', async () => {
    // Second GET (the post-save reload) never resolves until told to.
    let resolveReload!: (cfg: AzureFoundryPublicConfig) => void
    api.getAzureFoundryConfig
      .mockResolvedValueOnce({configured: false})
      .mockImplementationOnce(() => new Promise<AzureFoundryPublicConfig>(resolve => (resolveReload = resolve)))
    await renderUnconfigured()

    fireEvent.change(endpointField(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
      target: {value: 'gpt-4.1'},
    })
    fireEvent.click(saveButton())
    await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))

    // Neither the empty form (the stale unconfigured view) nor a stale summary
    // may flash while the reload is pending.
    await waitFor(() => expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull())
    expect(screen.queryByRole('button', {name: 'Save'})).toBeNull()

    resolveReload(apiKeyConfig)
    expect(await screen.findByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Edit'})).toBeInTheDocument()
  })

  it('posts Entra ID credentials together with the project endpoint', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    fireEvent.change(screen.getByLabelText('Azure client secret'), {target: {value: 's3cret'}})
    // Both ids are required by the server; do not let the click through early.
    expect(saveButton()).toBeDisabled()
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})
    // Entra ID without a project endpoint cannot list deployments either: a
    // chat deployment is still required.
    expect(saveButton()).toBeDisabled()
    fireEvent.change(screen.getByRole('textbox', {name: 'Project endpoint (optional)'}), {
      target: {value: 'https://my-res.services.ai.azure.com/api/projects/my-project'},
    })
    // With a project endpoint the project's deployments are listed.
    expect(saveButton()).toBeEnabled()
    fireEvent.click(saveButton())

    await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
    expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
      endpoint: 'https://my-res.services.ai.azure.com',
      auth: {mode: 'entra', tenantId: TENANT_GUID, clientId: CLIENT_GUID, clientSecret: 's3cret'},
      projectEndpoint: 'https://my-res.services.ai.azure.com/api/projects/my-project',
    })
  })

  it('summarises a configured provider with Edit, Remove and Test', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(entraConfig)
    render(<AzureFoundrySection />)
    expect(await screen.findByText('https://my-res.services.ai.azure.com')).toBeInTheDocument()
    expect(screen.getByText('Microsoft Entra ID')).toBeInTheDocument()
    expect(screen.getByText('https://my-res.services.ai.azure.com/api/projects/my-project')).toBeInTheDocument()
    expect(screen.getByText('text-embedding-3-small')).toBeInTheDocument()
    // Entra setups store a client secret, not an API key — the summary says so.
    expect(
      screen.getByText('Microsoft Entra ID (client secret) configured (stored encrypted on this server)'),
    ).toBeInTheDocument()
    for (const name of ['Edit', 'Remove', 'Test']) {
      expect(screen.getByRole('button', {name})).toBeInTheDocument()
    }
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
  })

  it('says the secret lives in the OS keychain on a local backend', async () => {
    useSystemStore.setState({info: info(true), isLoaded: true})
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    expect(await screen.findByText('API key configured (saved in your OS keychain)')).toBeInTheDocument()
  })

  it('says an Entra client secret lives in the OS keychain on a local backend', async () => {
    useSystemStore.setState({info: info(true), isLoaded: true})
    api.getAzureFoundryConfig.mockResolvedValue(entraConfig)
    render(<AzureFoundrySection />)
    expect(
      await screen.findByText('Microsoft Entra ID (client secret) configured (saved in your OS keychain)'),
    ).toBeInTheDocument()
  })

  it('reports a passing connection test with its latency', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Valid · 12 ms')).toBeInTheDocument()
    expect(api.testProviderConnection).toHaveBeenCalledWith('azure-foundry')
    // The pass is announced politely through the persistent status region;
    // nothing is raised as an alert.
    expect(screen.getByRole('status')).toHaveTextContent('Valid · 12 ms')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it("shows the server's reason when the connection test fails", async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    api.testProviderConnection.mockResolvedValue({
      ok: false,
      latencyMs: 0,
      error: 'deployment "gpt-4.1" was not found on this resource',
    })
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Invalid')).toBeInTheDocument()
    expect(screen.getByText('deployment "gpt-4.1" was not found on this resource')).toBeInTheDocument()
    // "Invalid" and the reason are announced together as one alert.
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Invalid')
    expect(alert).toHaveTextContent('deployment "gpt-4.1" was not found on this resource')
    expect(screen.getByRole('status')).toBeEmptyDOMElement()
  })

  it('explains a connection test whose body never arrived', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    api.testProviderConnection.mockResolvedValue(null as unknown as ProviderTestResult)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
    // A null body is a generic client-side failure with copy, not a bare
    // "Invalid" with no reason.
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Invalid')
    expect(alert).toHaveTextContent('The connection test failed')
    expect(screen.getByRole('status')).toBeEmptyDOMElement()
  })

  it('flags a non-Azure endpoint inline but lets the server rule on it', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://evil.example.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
      target: {value: 'gpt-4.1'},
    })
    // Not blurred yet: the error stays hidden.
    expect(screen.queryByText(/not an Azure AI Foundry endpoint/)).toBeNull()
    fireEvent.blur(endpointField())
    expect(screen.getByText(/not an Azure AI Foundry endpoint/)).toBeInTheDocument()
    expect(endpointField()).toHaveAttribute('aria-invalid', 'true')
    // The suffix hint does not block Save: operators can allow-list custom
    // hosts server-side, so the endpoint is posted and the backend decides.
    expect(saveButton()).toBeEnabled()
    fireEvent.click(saveButton())
    await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
  })

  it('requires an absolute https URL for the endpoint', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'http://my-res.openai.azure.com'}})
    fireEvent.blur(endpointField())
    expect(screen.getByText('The endpoint must use https://')).toBeInTheDocument()

    fireEvent.change(endpointField(), {target: {value: 'my-res.openai.azure.com'}})
    fireEvent.blur(endpointField())
    expect(screen.getByText('The endpoint must be an absolute https:// URL')).toBeInTheDocument()
    expect(saveButton()).toBeDisabled()

    fireEvent.change(endpointField(), {target: {value: 'https://my-res.openai.azure.com'}})
    expect(screen.queryByText('The endpoint must be an absolute https:// URL')).toBeNull()
  })

  it('flags non-GUID Entra IDs inline and blocks Save until they look like GUIDs', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: 'tenant-guid'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})
    fireEvent.change(screen.getByLabelText('Azure client secret'), {target: {value: 's3cret'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Project endpoint (optional)'}), {
      target: {value: 'https://my-res.services.ai.azure.com/api/projects/my-project'},
    })
    fireEvent.blur(screen.getByRole('textbox', {name: 'Tenant ID'}))
    expect(screen.getByText(/Must be a GUID/)).toBeInTheDocument()
    expect(saveButton()).toBeDisabled()

    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    expect(screen.queryByText(/Must be a GUID/)).toBeNull()
    expect(saveButton()).toBeEnabled()
  })

  it('maps HOST_NOT_ALLOWED to the host copy and shows other server messages verbatim', async () => {
    api.saveAzureFoundryConfig.mockRejectedValueOnce(new ApiError('host rejected', 400, 'HOST_NOT_ALLOWED'))
    await renderUnconfigured()
    // A host the client's suffix list accepts (the operator's server-side
    // allow-list decides) so the server's mapping stays exercised.
    fireEvent.change(endpointField(), {target: {value: 'https://other.openai.azure.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
      target: {value: 'gpt-4.1'},
    })
    fireEvent.click(saveButton())
    expect(await screen.findByText(/not an Azure AI Foundry endpoint/)).toBeInTheDocument()
    // The rejection is announced: the form relies on the server's validation.
    expect(screen.getByRole('alert')).toHaveTextContent(/not an Azure AI Foundry endpoint/)
    // The form and its values survive a rejected save; nothing is reloaded.
    expect(endpointField()).toHaveValue('https://other.openai.azure.com')
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(1)

    api.saveAzureFoundryConfig.mockRejectedValueOnce(
      new ApiError('deployment name "bad name" is invalid', 400, 'INVALID_DEPLOYMENT'),
    )
    fireEvent.click(saveButton())
    expect(await screen.findByText('deployment name "bad name" is invalid')).toBeInTheDocument()
    expect(screen.queryByText(/not an Azure AI Foundry endpoint/)).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('deployment name "bad name" is invalid')

    api.saveAzureFoundryConfig.mockRejectedValueOnce(
      new ApiError('name at least one chat deployment', 400, 'MISSING_DEPLOYMENT'),
    )
    fireEvent.click(saveButton())
    expect(await screen.findByText(/Name a default chat deployment or additional deployments/)).toBeInTheDocument()
    expect(screen.queryByText('name at least one chat deployment')).toBeNull()
  })

  it("keeps the server's text for a rejected project endpoint or an unknown field", async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
      target: {value: 'gpt-4.1'},
    })
    fireEvent.change(screen.getByRole('textbox', {name: 'Project endpoint (optional)'}), {
      target: {value: 'https://elsewhere.example.com/api/projects/p'},
    })

    // The project endpoint's host is at fault, not the resource endpoint's:
    // the HOST_NOT_ALLOWED copy would blame a resource endpoint that is fine.
    api.saveAzureFoundryConfig.mockRejectedValueOnce(
      new ApiError('project endpoint host "elsewhere.example.com" is not allowed', 400, 'INVALID_PROJECT_ENDPOINT'),
    )
    fireEvent.click(saveButton())
    expect(await screen.findByText('project endpoint host "elsewhere.example.com" is not allowed')).toBeInTheDocument()
    expect(screen.queryByText(/not an Azure AI Foundry endpoint/)).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('project endpoint host "elsewhere.example.com"')

    api.saveAzureFoundryConfig.mockRejectedValueOnce(
      new ApiError('unknown field "chatDeployments"', 400, 'INVALID_CONFIG'),
    )
    fireEvent.click(saveButton())
    expect(await screen.findByText('unknown field "chatDeployments"')).toBeInTheDocument()
    expect(screen.queryByText('project endpoint host "elsewhere.example.com" is not allowed')).toBeNull()
    expect(screen.queryByText('Failed to save the Azure AI Foundry settings')).toBeNull()
    // The form and its values survive the rejected saves.
    expect(endpointField()).toHaveValue('https://my-res.services.ai.azure.com')
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(1)
  })

  it('needs a chat deployment with an API key and sends the additional deployments as a clean list', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    // An API key cannot list deployments: nothing named, nothing to save.
    expect(saveButton()).toBeDisabled()
    expect(screen.getByText(/Name a default chat deployment or additional deployments/)).toBeInTheDocument()

    const extra = screen.getByRole('textbox', {name: 'Additional deployments (optional)'})
    expect(extra).toHaveAccessibleDescription(/Comma-separated deployment names/)
    fireEvent.change(extra, {target: {value: ' , ,'}})
    expect(saveButton()).toBeDisabled()
    fireEvent.change(extra, {target: {value: ' gpt-4o-mini , O4-mini,,GPT-4O-MINI, o4-mini '}})
    expect(saveButton()).toBeEnabled()
    expect(screen.queryByText(/Name a default chat deployment or additional deployments/)).toBeNull()
    fireEvent.click(saveButton())

    await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
    expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
      endpoint: 'https://my-res.openai.azure.com',
      auth: {mode: 'api_key', key: 'sk-test'},
      deployments: ['gpt-4o-mini', 'O4-mini'],
    })
  })

  it('lists, prefills and re-sends the stored additional deployments', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValue({...apiKeyConfig, deployments: ['gpt-4o-mini', 'model-router']})
    render(<AzureFoundrySection onConfigured={onConfigured} />)
    expect(await screen.findByText('Additional deployments')).toBeInTheDocument()
    expect(screen.getByText('gpt-4o-mini, model-router')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    expect(screen.getByRole('textbox', {name: 'Additional deployments (optional)'})).toHaveValue(
      'gpt-4o-mini, model-router',
    )
    fireEvent.click(saveButton())
    await waitFor(() => expect(onConfigured).toHaveBeenCalledTimes(1))
    expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
      endpoint: 'https://my-res.openai.azure.com',
      auth: {mode: 'api_key'},
      chatDeployment: 'gpt-4.1',
      deployments: ['gpt-4o-mini', 'model-router'],
    })
  })

  it('edits the public fields without re-entering the stored secret', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection onConfigured={onConfigured} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))

    expect(endpointField()).toHaveValue('https://my-res.openai.azure.com')
    expect(screen.getByRole('textbox', {name: 'Default chat deployment'})).toHaveValue('gpt-4.1')
    const secret = screen.getByLabelText('Azure API key')
    expect(secret).toHaveValue('')
    expect(secret).toHaveAttribute('placeholder', 'Leave blank to keep the stored secret')
    expect(saveButton()).toBeEnabled()
    expect(screen.getByRole('button', {name: 'Cancel'})).toBeInTheDocument()

    fireEvent.change(screen.getByRole('textbox', {name: 'Embedding deployment (optional)'}), {
      target: {value: 'text-embedding-3-small'},
    })
    fireEvent.click(saveButton())
    await waitFor(() => expect(onConfigured).toHaveBeenCalledTimes(1))
    const body = api.saveAzureFoundryConfig.mock.calls[0][0]
    expect(body).toStrictEqual({
      endpoint: 'https://my-res.openai.azure.com',
      auth: {mode: 'api_key'},
      chatDeployment: 'gpt-4.1',
      embeddingDeployment: 'text-embedding-3-small',
    })
    expect(body.auth).not.toHaveProperty('key')
    // Back on the summary after the reload.
    expect(screen.getByRole('button', {name: 'Edit'})).toBeInTheDocument()
  })

  it('needs a fresh secret when the auth mode changes while editing', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: TENANT_GUID}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: CLIENT_GUID}})

    const secret = screen.getByLabelText('Azure client secret')
    // The stored API key cannot stand in for a client secret.
    expect(secret).not.toHaveAttribute('placeholder', 'Leave blank to keep the stored secret')
    expect(saveButton()).toBeDisabled()
    fireEvent.change(secret, {target: {value: 's3cret'}})
    expect(saveButton()).toBeEnabled()
  })

  it('cancels a pristine edit back to the summary without asking', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    expect(screen.getByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
    expect(confirmMock).not.toHaveBeenCalled()
    expect(api.saveAzureFoundryConfig).not.toHaveBeenCalled()
  })

  it('discards a dirty edit only after a confirm', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
    fireEvent.change(endpointField(), {target: {value: 'https://other.openai.azure.com'}})
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))

    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(confirmMock).toHaveBeenCalledWith(
      expect.objectContaining({title: 'Discard unsaved changes?', confirmLabel: 'Discard', danger: true}),
    )
    expect(screen.getByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
    expect(api.saveAzureFoundryConfig).not.toHaveBeenCalled()
  })

  it('keeps a dirty edit when the discard confirm is rejected', async () => {
    confirmMock.mockResolvedValueOnce(false)
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
    fireEvent.change(endpointField(), {target: {value: 'https://other.openai.azure.com'}})
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))

    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    // The form and its edits survive a rejected discard.
    expect(endpointField()).toHaveValue('https://other.openai.azure.com')
    expect(saveButton()).toBeInTheDocument()
  })

  it('asks before removing the configuration', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValueOnce(apiKeyConfig).mockResolvedValue({configured: false})
    render(<AzureFoundrySection onConfigured={onConfigured} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))

    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(confirmMock).toHaveBeenCalledWith(
      expect.objectContaining({
        title: 'Remove Azure AI Foundry configuration',
        confirmLabel: 'Remove',
        danger: true,
      }),
    )
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toHaveValue('')
    expect(api.deleteApiKey).toHaveBeenCalledWith('azure-foundry')
    expect(onConfigured).toHaveBeenCalledTimes(1)
  })

  it('keeps the configuration when the remove confirm is rejected', async () => {
    confirmMock.mockResolvedValueOnce(false)
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))

    await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
    expect(api.deleteApiKey).not.toHaveBeenCalled()
    expect(screen.getByText('https://my-res.openai.azure.com')).toBeInTheDocument()
  })

  it('disables the summary actions while a removal is pending', async () => {
    let resolveDelete!: () => void
    api.deleteApiKey.mockImplementationOnce(() => new Promise<void>(resolve => (resolveDelete = resolve)))
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
    await waitFor(() => expect(api.deleteApiKey).toHaveBeenCalledTimes(1))

    // While the removal is pending every summary action is disabled.
    expect(screen.getByRole('button', {name: 'Removing…'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Edit'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Test'})).toBeDisabled()

    resolveDelete()
    await waitFor(() => expect(screen.getByRole('button', {name: 'Remove'})).toBeEnabled())
  })

  it('surfaces a failed removal instead of swallowing it', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    api.deleteApiKey.mockRejectedValueOnce(new Error('network down'))
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))

    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to remove the Azure AI Foundry configuration')
    // The failure is visible and the actions come back.
    expect(screen.getByRole('button', {name: 'Remove'})).toBeEnabled()
    expect(screen.getByRole('button', {name: 'Edit'})).toBeEnabled()
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(1)
  })

  it('prefers the server message when a removal fails with an ApiError', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    api.deleteApiKey.mockRejectedValueOnce(new ApiError('keyring unavailable', 500, null))
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))

    expect(await screen.findByRole('alert')).toHaveTextContent('keyring unavailable')
    expect(screen.getByRole('button', {name: 'Remove'})).toBeEnabled()
  })

  it('shows a load error with Retry instead of the empty form when the GET fails', async () => {
    api.getAzureFoundryConfig.mockRejectedValueOnce(new ApiError('boom', 500, null))
    render(<AzureFoundrySection />)

    // A failed GET must not render as "not configured": the empty form would
    // invite saving over the stored credentials.
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load the Azure AI Foundry configuration')
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()

    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toBeInTheDocument()
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(2)
  })

  it('falls back to the empty form when the config loads as unconfigured', async () => {
    api.getAzureFoundryConfig.mockResolvedValue({configured: false})
    render(<AzureFoundrySection />)
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toBeInTheDocument()
  })

  describe('Claude deployments', () => {
    const claudeField = () => screen.getByRole('textbox', {name: 'Claude deployments (optional)'})

    // Fills the required fields; the Claude field is left to the caller.
    async function fillRequired() {
      await renderUnconfigured()
      fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
      fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
      fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
        target: {value: 'gpt-4.1'},
      })
    }

    it('omits the field from the body when it is blank', async () => {
      await fillRequired()
      expect(claudeField()).toHaveAccessibleDescription(/Anthropic Messages route/)
      fireEvent.change(claudeField(), {target: {value: ' , '}})
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'api_key', key: 'sk-test'},
        chatDeployment: 'gpt-4.1',
      })
    })

    it('sends the Claude deployments as a clean list', async () => {
      await fillRequired()
      fireEvent.change(claudeField(), {target: {value: ' claude-sonnet-4-6 , claude-opus-4-8,, CLAUDE-SONNET-4-6 '}})
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'api_key', key: 'sk-test'},
        chatDeployment: 'gpt-4.1',
        claudeDeployments: ['claude-sonnet-4-6', 'claude-opus-4-8'],
      })
    })

    it('counts a Claude deployment as the required chat target', async () => {
      await renderUnconfigured()
      fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
      fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
      // No OpenAI-route deployment named: still blocked, and the copy now
      // offers Claude deployments as a way out.
      expect(saveButton()).toBeDisabled()
      expect(
        screen.getByText(/or Claude deployments, which answer on the Anthropic Messages route/),
      ).toBeInTheDocument()

      fireEvent.change(claudeField(), {target: {value: 'claude-sonnet-4-6'}})
      expect(saveButton()).toBeEnabled()
      expect(screen.queryByText(/or Claude deployments, which answer on the Anthropic Messages route/)).toBeNull()
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'api_key', key: 'sk-test'},
        claudeDeployments: ['claude-sonnet-4-6'],
      })
    })

    it('drops Claude names already listed as another deployment, and says so', async () => {
      await fillRequired()
      fireEvent.change(screen.getByRole('textbox', {name: 'Additional deployments (optional)'}), {
        target: {value: 'gpt-4o-mini'},
      })
      fireEvent.change(claudeField(), {target: {value: 'GPT-4.1, claude-sonnet-4-6, gpt-4o-mini'}})
      expect(
        screen.getByText(/GPT-4\.1, gpt-4o-mini are already listed as chat, embedding or additional deployments/),
      ).toBeInTheDocument()
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
        endpoint: 'https://my-res.services.ai.azure.com',
        auth: {mode: 'api_key', key: 'sk-test'},
        chatDeployment: 'gpt-4.1',
        deployments: ['gpt-4o-mini'],
        claudeDeployments: ['claude-sonnet-4-6'],
      })
    })

    it('does not count a duplicated Claude name as the chat target', async () => {
      await renderUnconfigured()
      fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
      fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
      fireEvent.change(screen.getByRole('textbox', {name: 'Embedding deployment (optional)'}), {
        target: {value: 'text-embedding-3-small'},
      })
      fireEvent.change(claudeField(), {target: {value: 'text-embedding-3-small'}})
      expect(screen.getByText(/is already listed as a chat, embedding or additional deployment/)).toBeInTheDocument()
      expect(saveButton()).toBeDisabled()
    })

    it('lists, prefills and re-sends the stored Claude deployments', async () => {
      api.getAzureFoundryConfig.mockResolvedValue({
        ...apiKeyConfig,
        claudeDeployments: ['claude-sonnet-4-6', 'claude-opus-4-8'],
      })
      render(<AzureFoundrySection />)
      expect(await screen.findByText('Claude deployments')).toBeInTheDocument()
      expect(screen.getByText('claude-sonnet-4-6, claude-opus-4-8')).toBeInTheDocument()

      fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
      expect(claudeField()).toHaveValue('claude-sonnet-4-6, claude-opus-4-8')
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
        endpoint: 'https://my-res.openai.azure.com',
        auth: {mode: 'api_key'},
        chatDeployment: 'gpt-4.1',
        claudeDeployments: ['claude-sonnet-4-6', 'claude-opus-4-8'],
      })
    })
  })

  describe('org scope', () => {
    const sharedConfig: AzureFoundryPublicConfig = {
      ...apiKeyConfig,
      shared: true,
      updatedBy: 'Ada Admin',
      updatedAt: '2026-09-28T10:00:00Z',
    }
    const admin = {kind: 'org', orgId: 'org-1', canEdit: true} as const
    const member = {kind: 'org', orgId: 'org-1', canEdit: false} as const

    it('reads, saves, tests and removes through the org routes for an admin', async () => {
      const onConfigured = vi.fn()
      api.getOrgAzureFoundryConfig.mockResolvedValueOnce({configured: false}).mockResolvedValue(sharedConfig)
      render(<AzureFoundrySection scope={admin} onConfigured={onConfigured} />)
      await screen.findByRole('textbox', {name: 'Azure endpoint'})
      expect(api.getOrgAzureFoundryConfig).toHaveBeenCalledWith('org-1')
      expect(api.getAzureFoundryConfig).not.toHaveBeenCalled()

      fireEvent.change(endpointField(), {target: {value: 'https://my-res.openai.azure.com'}})
      fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-org'}})
      fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {
        target: {value: 'gpt-4.1'},
      })
      fireEvent.click(saveButton())
      await waitFor(() => expect(onConfigured).toHaveBeenCalledTimes(1))
      expect(api.saveOrgAzureFoundryConfig).toHaveBeenCalledWith('org-1', {
        endpoint: 'https://my-res.openai.azure.com',
        auth: {mode: 'api_key', key: 'sk-org'},
        chatDeployment: 'gpt-4.1',
      })
      expect(api.saveAzureFoundryConfig).not.toHaveBeenCalled()

      // The summary names who set it and keeps the admin's actions.
      expect(screen.getByText('Shared by your organisation · set by Ada Admin')).toBeInTheDocument()
      expect(screen.queryByText('Only organisation admins can change the shared configuration.')).toBeNull()
      fireEvent.click(screen.getByRole('button', {name: 'Test'}))
      expect(await screen.findByText('Valid · 34 ms')).toBeInTheDocument()
      expect(api.testOrgAzureFoundryConnection).toHaveBeenCalledWith('org-1')
      expect(api.testProviderConnection).not.toHaveBeenCalled()

      api.getOrgAzureFoundryConfig.mockResolvedValue({configured: false})
      fireEvent.click(screen.getByRole('button', {name: 'Remove'}))
      expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toHaveValue('')
      expect(api.deleteOrgAzureFoundryConfig).toHaveBeenCalledWith('org-1')
      expect(api.deleteApiKey).not.toHaveBeenCalled()
    })

    it('shows a member the read-only summary without any actions', async () => {
      api.getOrgAzureFoundryConfig.mockResolvedValue(sharedConfig)
      render(<AzureFoundrySection scope={member} />)
      expect(await screen.findByText('https://my-res.openai.azure.com')).toBeInTheDocument()
      expect(screen.getByText('gpt-4.1')).toBeInTheDocument()
      expect(screen.getByText('Shared by your organisation · set by Ada Admin')).toBeInTheDocument()
      expect(screen.getByText('Only organisation admins can change the shared configuration.')).toBeInTheDocument()
      expect(screen.queryByRole('button')).toBeNull()
      expect(screen.queryByRole('textbox')).toBeNull()
    })

    it('falls back to a nameless shared line when the server omits updatedBy', async () => {
      api.getOrgAzureFoundryConfig.mockResolvedValue({...apiKeyConfig, shared: true})
      render(<AzureFoundrySection scope={member} />)
      expect(await screen.findByText('Shared by your organisation')).toBeInTheDocument()
    })

    it('tells a member when nothing is shared instead of offering the form', async () => {
      api.getOrgAzureFoundryConfig.mockResolvedValue({configured: false})
      render(<AzureFoundrySection scope={member} />)
      expect(
        await screen.findByText('Your organisation has not set up a shared Azure AI Foundry resource.'),
      ).toBeInTheDocument()
      expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
      expect(screen.queryByRole('button', {name: 'Save'})).toBeNull()
    })

    it('keeps the load-error row (with Retry) for a member', async () => {
      api.getOrgAzureFoundryConfig.mockRejectedValueOnce(new ApiError('boom', 500, null))
      render(<AzureFoundrySection scope={member} />)
      expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load the Azure AI Foundry configuration')
      fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
      expect(
        await screen.findByText('Your organisation has not set up a shared Azure AI Foundry resource.'),
      ).toBeInTheDocument()
      expect(api.getOrgAzureFoundryConfig).toHaveBeenCalledTimes(2)
    })

    it('sends the updatedAt it read as a compare-and-swap guard, and shows when it was updated', async () => {
      api.getOrgAzureFoundryConfig.mockResolvedValue(sharedConfig)
      render(<AzureFoundrySection scope={admin} />)
      const updated = await screen.findByText(/^updated /)
      expect(updated.tagName).toBe('TIME')
      expect(updated).toHaveAttribute('dateTime', '2026-09-28T10:00:00Z')

      fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
      fireEvent.click(saveButton())
      await waitFor(() => expect(api.saveOrgAzureFoundryConfig).toHaveBeenCalledTimes(1))
      expect(api.saveOrgAzureFoundryConfig).toHaveBeenCalledWith('org-1', {
        endpoint: 'https://my-res.openai.azure.com',
        auth: {mode: 'api_key'},
        chatDeployment: 'gpt-4.1',
        updatedAt: '2026-09-28T10:00:00Z',
      })
    })

    it("explains a concurrent change (409) and reloads the other admin's version on request", async () => {
      api.getOrgAzureFoundryConfig.mockResolvedValue(sharedConfig)
      api.saveOrgAzureFoundryConfig.mockRejectedValue(new ApiError('changed', 409, 'CONFLICT'))
      render(<AzureFoundrySection scope={admin} />)
      fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
      fireEvent.click(saveButton())
      const alert = await screen.findByText(/Someone else changed the shared configuration/)
      expect(alert.closest('[role="alert"]')).not.toBeNull()

      api.getOrgAzureFoundryConfig.mockResolvedValue({...sharedConfig, chatDeployment: 'gpt-5', updatedBy: 'Bo'})
      fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
      expect(await screen.findByText('gpt-5')).toBeInTheDocument()
      expect(screen.getByText('Shared by your organisation · set by Bo')).toBeInTheDocument()
      expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
    })

    it('uses permission copy for a 403 on save, remove and test', async () => {
      const forbidden = new ApiError('forbidden', 403, null)
      api.getOrgAzureFoundryConfig.mockResolvedValue(sharedConfig)
      api.testOrgAzureFoundryConnection.mockRejectedValue(forbidden)
      api.deleteOrgAzureFoundryConfig.mockRejectedValue(forbidden)
      api.saveOrgAzureFoundryConfig.mockRejectedValue(forbidden)
      render(<AzureFoundrySection scope={admin} />)
      const readOnly = 'Only organisation admins can change the shared configuration.'

      fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
      expect(await screen.findByText(readOnly)).toBeInTheDocument()
      fireEvent.click(screen.getByRole('button', {name: 'Remove'}))
      await waitFor(() => expect(screen.getAllByText(readOnly)).toHaveLength(2))
      expect(screen.queryByText('forbidden')).toBeNull()

      fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
      fireEvent.click(saveButton())
      expect(await screen.findByRole('alert')).toHaveTextContent(readOnly)
    })

    it('shows a note, not a dead Retry, when the caller may not read the shared configuration', async () => {
      api.getOrgAzureFoundryConfig.mockRejectedValue(new ApiError('forbidden', 403, null))
      render(<AzureFoundrySection scope={member} />)
      expect(await screen.findByText(/Only organisation admins and members can use/)).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: 'Retry'})).toBeNull()
    })

    it('renders nothing and tells the host when the deployment has no org keystore', async () => {
      const onUnavailable = vi.fn()
      api.getOrgAzureFoundryConfig.mockRejectedValue(new ApiError('no keystore', 404, 'ORG_KEYSTORE_UNAVAILABLE'))
      const {container} = render(<AzureFoundrySection scope={admin} onUnavailable={onUnavailable} />)
      await waitFor(() => expect(onUnavailable).toHaveBeenCalledTimes(1))
      expect(container).toBeEmptyDOMElement()
    })

    it('says an unreadable stored credential is unreadable, and lets an admin enter it again', async () => {
      api.getOrgAzureFoundryConfig.mockRejectedValue(new ApiError('decrypt', 500, 'ORG_CREDENTIAL_UNREADABLE'))
      render(<AzureFoundrySection scope={admin} />)
      expect(await screen.findByRole('alert')).toHaveTextContent(/stored but could not be read/)
      expect(screen.getByRole('button', {name: 'Retry'})).toBeInTheDocument()
      expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()

      fireEvent.click(screen.getByRole('button', {name: 'Enter again'}))
      expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toHaveValue('')
      expect(screen.queryByRole('alert')).toBeNull()
    })

    it('never offers a member the replace action for an unreadable credential', async () => {
      api.getOrgAzureFoundryConfig.mockRejectedValue(new ApiError('decrypt', 500, 'ORG_CREDENTIAL_UNREADABLE'))
      render(<AzureFoundrySection scope={member} />)
      expect(await screen.findByRole('alert')).toHaveTextContent(/stored but could not be read/)
      expect(screen.queryByRole('button', {name: 'Enter again'})).toBeNull()
    })
  })

  it('keeps a "saved, refresh failed" state instead of the empty form when the reload fails', async () => {
    const onConfigured = vi.fn()
    await renderUnconfigured(onConfigured)
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.openai.azure.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment'}), {target: {value: 'gpt-4.1'}})
    api.getAzureFoundryConfig.mockRejectedValueOnce(new Error('network'))
    fireEvent.click(saveButton())

    expect(
      await screen.findByText("Your change was saved, but the configuration couldn't be reloaded."),
    ).toBeInTheDocument()
    expect(onConfigured).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()

    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.queryByText(/couldn't be reloaded/)).toBeNull()
  })
})
