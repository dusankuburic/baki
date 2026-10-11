import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
import CredentialForm from './CredentialForm'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import {useSystemStore} from '@/stores/systemStore'
import {API_KEY_FIELD, deviceFlowSpec, providerSpec} from '@/testing/fixtures/providerCatalog'
import type {ProviderSpec} from '@/types'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      saveApiKey: vi.fn(),
      hasApiKey: vi.fn(),
      deleteApiKey: vi.fn(),
      testProviderConnection: vi.fn(),
      getDeviceAuthUser: vi.fn(),
      startDeviceAuth: vi.fn(),
      pollDeviceAuth: vi.fn(),
      revokeDeviceAuth: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)
const spec = (over: Parameters<typeof providerSpec>[0]) => providerSpec(over) as unknown as ProviderSpec

beforeEach(() => {
  vi.resetAllMocks()
  useSystemStore.setState({info: null, isLoaded: false})
  api.hasApiKey.mockResolvedValue(false)
  api.saveApiKey.mockResolvedValue(undefined)
  api.deleteApiKey.mockResolvedValue(undefined)
  api.getDeviceAuthUser.mockResolvedValue(null)
})

describe('CredentialForm: api_key', () => {
  it('submits {provider, key} to /api/keys/save for an api_key schema', async () => {
    const onConfigured = vi.fn()
    render(<CredentialForm spec={spec({id: 'mistral', name: 'Mistral'})} onConfigured={onConfigured} />)
    const key = await screen.findByLabelText('API key')
    expect(key).toHaveAttribute('type', 'password')
    expect(key).toHaveAttribute('placeholder', 'sk-…')
    expect(screen.getByRole('button', {name: 'Save'})).toBeDisabled()
    fireEvent.change(key, {target: {value: ' sk-1 '}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(api.saveApiKey).toHaveBeenCalledWith('mistral', 'sk-1'))
    expect(await screen.findByText(/API key configured/)).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Test'})).toBeInTheDocument()
    expect(onConfigured).toHaveBeenCalledTimes(1)
  })

  it('requires every required field before Save enables, and sends the extra fields by name', async () => {
    render(
      <CredentialForm
        spec={spec({
          id: 'mistral',
          credential: {
            kind: 'api_key',
            fields: [
              API_KEY_FIELD,
              {name: 'region', label: 'Region', type: 'select', required: true, options: [{value: 'eu', label: 'EU'}]},
              {name: 'endpoint', label: 'Endpoint', type: 'url', required: false, help: 'Optional.'},
            ],
          },
        })}
        onConfigured={vi.fn()}
      />,
    )
    const save = await screen.findByRole('button', {name: 'Save'})
    fireEvent.change(screen.getByLabelText('API key'), {target: {value: 'sk-1'}})
    expect(save).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Region'), {target: {value: 'eu'}})
    expect(save).toBeEnabled()
    expect(screen.getByLabelText('Endpoint')).toHaveAccessibleDescription('Optional.')
    fireEvent.click(save)
    await waitFor(() => expect(api.saveApiKey).toHaveBeenCalledWith('mistral', 'sk-1', {region: 'eu'}))
  })

  it('keeps a blank secret on a configured provider as "unchanged": Replace key posts nothing until a key is typed', async () => {
    api.hasApiKey.mockResolvedValue(true)
    render(<CredentialForm spec={spec({id: 'mistral', configured: true})} onConfigured={vi.fn()} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Replace key'}))
    const key = screen.getByLabelText('API key')
    expect(key).toHaveAttribute('placeholder', 'Leave blank to keep the stored key')
    expect(screen.getByRole('button', {name: 'Save'})).toBeDisabled()
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    expect(api.saveApiKey).not.toHaveBeenCalled()
    expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
  })

  it('shows the server code (USE_PROVIDER_CONFIG, PROVIDER_UNKNOWN) instead of a bare "save failed"', async () => {
    api.saveApiKey.mockRejectedValueOnce(
      new ApiError('this provider is configured through /api/providers/local/config', 400, 'USE_PROVIDER_CONFIG'),
    )
    render(<CredentialForm spec={spec({id: 'local'})} onConfigured={vi.fn()} />)
    fireEvent.change(await screen.findByLabelText('API key'), {target: {value: 'sk-1'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This provider is set up in its own section, not with a key (USE_PROVIDER_CONFIG)',
    )

    api.saveApiKey.mockRejectedValueOnce(new ApiError('unknown provider "local"', 400, 'PROVIDER_UNKNOWN'))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This server does not know this provider (PROVIDER_UNKNOWN)',
    )

    api.saveApiKey.mockRejectedValueOnce(new ApiError('keys are read-only here', 403, 'KEYS_READ_ONLY'))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('keys are read-only here (KEYS_READ_ONLY)')

    api.saveApiKey.mockRejectedValueOnce(new TypeError('fetch failed'))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to save')
  })

  it('reports the test outcome with the latency, or the server error and code', async () => {
    api.hasApiKey.mockResolvedValue(true)
    api.testProviderConnection.mockResolvedValueOnce({ok: true, latencyMs: 12})
    render(<CredentialForm spec={spec({id: 'mistral', configured: true})} onConfigured={vi.fn()} />)
    fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Valid · 12 ms')).toBeInTheDocument()
    api.testProviderConnection.mockResolvedValueOnce({
      ok: false,
      latencyMs: 0,
      error: 'budget exhausted',
      code: 'AI_BUDGET_EXCEEDED',
    })
    fireEvent.click(screen.getByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Invalid · budget exhausted (AI_BUDGET_EXCEEDED)')).toBeInTheDocument()
  })

  it('removes the key and says where it was stored', async () => {
    api.hasApiKey.mockResolvedValue(true)
    const onConfigured = vi.fn()
    render(<CredentialForm spec={spec({id: 'mistral', configured: true})} onConfigured={onConfigured} />)
    expect(await screen.findByText('API key configured (stored encrypted on this server)')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Remove'}))
    await waitFor(() => expect(api.deleteApiKey).toHaveBeenCalledWith('mistral'))
    expect(await screen.findByLabelText('API key')).toBeInTheDocument()
    expect(onConfigured).toHaveBeenCalledTimes(1)
  })

  it('offers Remove only once the key store has answered, and falls back to the catalogue when it cannot', async () => {
    let answer!: (has: boolean) => void
    api.hasApiKey.mockReturnValue(
      new Promise<boolean>(resolve => {
        answer = resolve
      }),
    )
    const pending = render(<CredentialForm spec={spec({id: 'claude', configured: true})} onConfigured={vi.fn()} />)
    await act(async () => {})
    expect(screen.queryByRole('button', {name: 'Remove'})).toBeNull()
    expect(screen.queryByText(/API key configured/)).toBeNull()
    expect(screen.queryByLabelText('API key')).toBeNull()
    await act(async () => answer(false))
    expect(await screen.findByLabelText('API key')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Remove'})).toBeNull()
    pending.unmount()

    api.hasApiKey.mockRejectedValue(new Error('offline'))
    render(<CredentialForm spec={spec({id: 'claude', configured: true})} onConfigured={vi.fn()} />)
    expect(await screen.findByText(/API key configured/)).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
  })

  it('shows the key form for a catalogue-configured provider whose key store answers false', async () => {
    api.hasApiKey.mockResolvedValue(false)
    render(<CredentialForm spec={spec({id: 'openai', configured: true})} onConfigured={vi.fn()} />)
    expect(await screen.findByLabelText('API key')).toBeInTheDocument()
    expect(screen.queryByText(/API key configured/)).toBeNull()
    expect(screen.queryByRole('button', {name: 'Remove'})).toBeNull()
  })

  it('posts the first secret field as the key whatever the catalogue calls it', async () => {
    render(
      <CredentialForm
        spec={spec({
          id: 'acme',
          credential: {
            kind: 'api_key',
            fields: [
              {name: 'token', label: 'Access token', type: 'secret', required: true},
              {name: 'key', label: 'Key id', type: 'text', required: true},
            ],
          },
        })}
        onConfigured={vi.fn()}
      />,
    )
    fireEvent.change(await screen.findByLabelText('Access token'), {target: {value: 'tok-1'}})
    fireEvent.change(screen.getByLabelText('Key id'), {target: {value: 'kid'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(api.saveApiKey).toHaveBeenCalledWith('acme', 'tok-1', {key: 'kid'}))
  })

  it('treats a field named after a prototype key as any other field', async () => {
    render(
      <CredentialForm
        spec={spec({
          id: 'acme',
          credential: {
            kind: 'api_key',
            fields: [API_KEY_FIELD, {name: 'constructor', label: 'Constructor', type: 'text', required: true}],
          },
        })}
        onConfigured={vi.fn()}
      />,
    )
    const save = await screen.findByRole('button', {name: 'Save'})
    expect(screen.getByLabelText('Constructor')).toHaveValue('')
    fireEvent.change(screen.getByLabelText('API key'), {target: {value: 'sk-1'}})
    expect(save).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Constructor'), {target: {value: 'c1'}})
    expect(save).toBeEnabled()
    fireEvent.click(save)
    await waitFor(() => expect(api.saveApiKey).toHaveBeenCalledWith('acme', 'sk-1', {constructor: 'c1'}))
  })
})

describe('CredentialForm: other kinds', () => {
  it('none: nothing to enter, Test only', async () => {
    api.testProviderConnection.mockResolvedValue({ok: true, latencyMs: 0})
    render(
      <CredentialForm
        spec={spec({id: 'free', credential: {kind: 'none', fields: []}, configured: true})}
        onConfigured={vi.fn()}
      />,
    )
    expect(screen.getByText('No credentials are needed.')).toBeInTheDocument()
    expect(document.querySelectorAll('input')).toHaveLength(0)
    expect(screen.queryByRole('button', {name: 'Remove'})).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: 'Test'}))
    expect(await screen.findByText('Valid')).toBeInTheDocument()
    expect(api.testProviderConnection).toHaveBeenCalledWith('free')
  })

  it('oauth_device: the device-flow button bound to the catalogue endpoints', async () => {
    api.startDeviceAuth.mockResolvedValue({
      device_code: 'dc',
      user_code: 'ABCD-1234',
      verification_uri: 'https://example.com/device',
      expires_in: 600,
      interval: 5,
    })
    render(
      <CredentialForm
        spec={spec({
          id: 'acme',
          name: 'Acme',
          authType: 'oauth',
          credential: {kind: 'oauth_device', fields: []},
          endpoints: {
            start: '/api/providers/acme/start',
            poll: '/api/providers/acme/poll',
            revoke: '/api/providers/acme/revoke',
            user: '/api/providers/acme/user',
          },
        })}
        onConfigured={vi.fn()}
      />,
    )
    await act(async () => {})
    expect(api.getDeviceAuthUser).toHaveBeenCalledWith('/api/providers/acme/user')
    fireEvent.click(screen.getByRole('button', {name: 'Sign in with Acme'}))
    expect(await screen.findByText('ABCD-1234')).toBeInTheDocument()
    expect(api.startDeviceAuth).toHaveBeenCalledWith('/api/providers/acme/start')
  })

  it('oauth_device without the start and poll routes, or with a route outside /api/, renders the generic card', async () => {
    render(
      <CredentialForm
        spec={spec({
          id: 'acme',
          name: 'Acme',
          credential: {kind: 'oauth_device', fields: []},
          endpoints: {start: 'https://evil.example/start', poll: '/api/providers/acme/poll'},
        })}
        onConfigured={vi.fn()}
      />,
    )
    expect(screen.getByText("This version of Baki can't show the set-up form for this provider.")).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /Sign in/})).toBeNull()
    expect(api.getDeviceAuthUser).not.toHaveBeenCalled()
    expect(api.startDeviceAuth).not.toHaveBeenCalled()
  })

  it('document without an override: the generic card, configured on the server', () => {
    render(
      <CredentialForm
        spec={spec({id: 'orbit', name: 'Orbit', credential: {kind: 'document', fields: []}, configured: true})}
        onConfigured={vi.fn()}
      />,
    )
    expect(screen.getByText('Configured')).toBeInTheDocument()
    expect(screen.getByText('Configured on the server.')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Test'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
    expect(document.querySelectorAll('input')).toHaveLength(0)
  })
})

describe('CredentialForm: oauth_device glyph', () => {
  it("the button's glyph is the catalogue icon, the GitHub mark for the GitHub ids, a key otherwise", async () => {
    const acme = render(<CredentialForm spec={spec(deviceFlowSpec('acme', {name: 'Acme'}))} onConfigured={vi.fn()} />)
    await act(async () => {})
    expect(screen.getByRole('button', {name: 'Sign in with Acme'})).toHaveAttribute('data-glyph', 'key')
    acme.unmount()

    const cloud = render(
      <CredentialForm spec={spec(deviceFlowSpec('acme', {name: 'Acme', icon: 'cloud'}))} onConfigured={vi.fn()} />,
    )
    await act(async () => {})
    expect(screen.getByRole('button', {name: 'Sign in with Acme'})).toHaveAttribute('data-glyph', 'cloud')
    cloud.unmount()

    render(<CredentialForm spec={spec(deviceFlowSpec('copilot', {name: 'Copilot'}))} onConfigured={vi.fn()} />)
    await act(async () => {})
    expect(screen.getByRole('button', {name: 'Sign in with GitHub'})).toHaveAttribute('data-glyph', 'github')
  })
})
