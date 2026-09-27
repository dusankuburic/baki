import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import AzureFoundrySection from './AzureFoundrySection'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import {useSystemStore} from '@/stores/systemStore'
import type {AppInfo, AzureFoundryPublicConfig} from '@/types'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      getAzureFoundryConfig: vi.fn(),
      saveAzureFoundryConfig: vi.fn(),
      deleteApiKey: vi.fn(),
      testProviderConnection: vi.fn(),
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
  useSystemStore.setState({info: info(false), isLoaded: true})
  api.getAzureFoundryConfig.mockResolvedValue({configured: false})
  api.saveAzureFoundryConfig.mockResolvedValue(undefined)
  api.deleteApiKey.mockResolvedValue(undefined)
  api.testProviderConnection.mockResolvedValue({ok: true, latencyMs: 12})
})

describe('AzureFoundrySection', () => {
  it('shows the empty form with API-key auth when nothing is configured', async () => {
    await renderUnconfigured()
    expect(screen.getByRole('radio', {name: 'API key'})).toHaveAttribute('aria-checked', 'true')
    const secret = screen.getByLabelText('Azure API key')
    expect(secret).toHaveAttribute('type', 'password')
    expect(secret).toHaveAttribute('autocomplete', 'off')
    expect(secret).toHaveAttribute('spellcheck', 'false')
    expect(screen.queryByRole('textbox', {name: 'Tenant ID'})).toBeNull()
    expect(saveButton()).toBeDisabled()
    expect(screen.queryByRole('button', {name: 'Cancel'})).toBeNull()

    fireEvent.click(screen.getByRole('button', {name: 'Show'}))
    expect(screen.getByLabelText('Azure API key')).toHaveAttribute('type', 'text')
    fireEvent.click(screen.getByRole('button', {name: 'Hide'}))
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
    fireEvent.change(screen.getByRole('textbox', {name: 'Default chat deployment (optional)'}), {
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

  it('posts Entra ID credentials together with the project endpoint', async () => {
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://my-res.services.ai.azure.com'}})
    fireEvent.click(screen.getByRole('radio', {name: 'Microsoft Entra ID'}))
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: 'tenant-guid'}})
    fireEvent.change(screen.getByLabelText('Azure client secret'), {target: {value: 's3cret'}})
    // Both ids are required by the server; do not let the click through early.
    expect(saveButton()).toBeDisabled()
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: 'client-guid'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Project endpoint (optional)'}), {
      target: {value: 'https://my-res.services.ai.azure.com/api/projects/my-project'},
    })
    fireEvent.click(saveButton())

    await waitFor(() => expect(api.saveAzureFoundryConfig).toHaveBeenCalledTimes(1))
    expect(api.saveAzureFoundryConfig.mock.calls[0][0]).toStrictEqual({
      endpoint: 'https://my-res.services.ai.azure.com',
      auth: {mode: 'entra', tenantId: 'tenant-guid', clientId: 'client-guid', clientSecret: 's3cret'},
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
    expect(screen.getByText('API key configured (stored encrypted on this server)')).toBeInTheDocument()
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

  it('reports a passing connection test', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Valid')).toBeInTheDocument()
    expect(api.testProviderConnection).toHaveBeenCalledWith('azure-foundry')
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
  })

  it('maps HOST_NOT_ALLOWED to the host copy and shows other server messages verbatim', async () => {
    api.saveAzureFoundryConfig.mockRejectedValueOnce(new ApiError('host rejected', 400, 'HOST_NOT_ALLOWED'))
    await renderUnconfigured()
    fireEvent.change(endpointField(), {target: {value: 'https://evil.example.com'}})
    fireEvent.change(screen.getByLabelText('Azure API key'), {target: {value: 'sk-test'}})
    fireEvent.click(saveButton())
    expect(await screen.findByText(/not an Azure AI Foundry endpoint/)).toBeInTheDocument()
    // The form and its values survive a rejected save; nothing is reloaded.
    expect(endpointField()).toHaveValue('https://evil.example.com')
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(1)

    api.saveAzureFoundryConfig.mockRejectedValueOnce(
      new ApiError('deployment name "bad name" is invalid', 400, 'INVALID_DEPLOYMENT'),
    )
    fireEvent.click(saveButton())
    expect(await screen.findByText('deployment name "bad name" is invalid')).toBeInTheDocument()
    expect(screen.queryByText(/not an Azure AI Foundry endpoint/)).toBeNull()
  })

  it('edits the public fields without re-entering the stored secret', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection onConfigured={onConfigured} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))

    expect(endpointField()).toHaveValue('https://my-res.openai.azure.com')
    expect(screen.getByRole('textbox', {name: 'Default chat deployment (optional)'})).toHaveValue('gpt-4.1')
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
    fireEvent.change(screen.getByRole('textbox', {name: 'Tenant ID'}), {target: {value: 'tenant-guid'}})
    fireEvent.change(screen.getByRole('textbox', {name: 'Client ID'}), {target: {value: 'client-guid'}})

    const secret = screen.getByLabelText('Azure client secret')
    // The stored API key cannot stand in for a client secret.
    expect(secret).not.toHaveAttribute('placeholder', 'Leave blank to keep the stored secret')
    expect(saveButton()).toBeDisabled()
    fireEvent.change(secret, {target: {value: 's3cret'}})
    expect(saveButton()).toBeEnabled()
  })

  it('cancels an edit back to the summary without saving', async () => {
    api.getAzureFoundryConfig.mockResolvedValue(apiKeyConfig)
    render(<AzureFoundrySection />)
    fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
    fireEvent.change(endpointField(), {target: {value: 'https://other.openai.azure.com'}})
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    expect(screen.getByText('https://my-res.openai.azure.com')).toBeInTheDocument()
    expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
    expect(api.saveAzureFoundryConfig).not.toHaveBeenCalled()
  })

  it('removes the configuration and returns to the empty form', async () => {
    const onConfigured = vi.fn()
    api.getAzureFoundryConfig.mockResolvedValueOnce(apiKeyConfig).mockResolvedValue({configured: false})
    render(<AzureFoundrySection onConfigured={onConfigured} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toHaveValue('')
    expect(api.deleteApiKey).toHaveBeenCalledWith('azure-foundry')
    expect(onConfigured).toHaveBeenCalledTimes(1)
  })

  it('falls back to the empty form when the config cannot be loaded', async () => {
    api.getAzureFoundryConfig.mockRejectedValue(new ApiError('not found', 404, null))
    render(<AzureFoundrySection />)
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toBeInTheDocument()
  })
})
