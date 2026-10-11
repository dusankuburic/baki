import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import LocalModelsSection from './LocalModelsSection'
import {providersApi} from '@/api'
import {ApiError, request} from '@/api/client'
import type {LocalModelsConfig} from '@/types'
import {formatCount} from '@/lib/format'

const confirmMock = vi.hoisted(() => vi.fn())
vi.mock('@/components/shared', async importOriginal => ({
  ...(await importOriginal<typeof import('@/components/shared')>()),
  useConfirm: () => ({confirm: confirmMock, prompt: vi.fn()}),
}))

vi.mock('@/api/client', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  request: vi.fn(),
}))

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      getLocalModelsConfig: vi.fn(),
      saveLocalModelsConfig: vi.fn(),
      deleteApiKey: vi.fn(),
      testProviderConnection: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)

const URL_OLLAMA = 'http://127.0.0.1:11434/v1'
const configured: LocalModelsConfig = {configured: true, baseUrl: URL_OLLAMA}
const empty: LocalModelsConfig = {configured: false, baseUrl: ''}
const unavailable = () =>
  new ApiError('local models are available in the desktop app only', 400, 'LOCAL_MODELS_UNAVAILABLE')
const WINDOW_INVALID = 'Enter a whole number between 1024 and 2000000'

const baseUrlField = () => screen.getByRole('textbox', {name: 'Base URL'})
const windowField = () => screen.getByRole('textbox', {name: 'Context window (tokens)'})
const saveButton = () => screen.getByRole('button', {name: 'Save'})

beforeEach(() => {
  vi.resetAllMocks()
  confirmMock.mockResolvedValue(true)
  api.getLocalModelsConfig.mockResolvedValue(empty)
})

describe('LocalModelsSection', () => {
  it('shows a skeleton until the config loads, then the empty form with a URL placeholder', async () => {
    render(<LocalModelsSection />)
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    const field = await screen.findByRole('textbox', {name: 'Base URL'})
    expect(field).toHaveAttribute('placeholder', URL_OLLAMA)
    expect(screen.queryByRole('button', {name: 'Cancel'})).not.toBeInTheDocument()
    expect(saveButton()).toBeDisabled()
  })

  it('rejects a value that is not an http(s) URL after blur-sm and keeps Save disabled', async () => {
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: 'localhost:11434'}})
    expect(screen.queryByText('Enter a full http:// or https:// URL')).not.toBeInTheDocument()
    fireEvent.blur(baseUrlField())
    expect(screen.getByText('Enter a full http:// or https:// URL')).toBeInTheDocument()
    expect(saveButton()).toBeDisabled()
  })

  it('saves the trimmed URL, reloads, notifies the host and lands focus on Edit', async () => {
    const onConfigured = vi.fn()
    api.getLocalModelsConfig.mockResolvedValueOnce(empty).mockResolvedValueOnce(configured)
    api.saveLocalModelsConfig.mockResolvedValue(undefined)
    render(<LocalModelsSection onConfigured={onConfigured} />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: `  ${URL_OLLAMA}  `}})
    fireEvent.click(saveButton())

    expect(await screen.findByText(URL_OLLAMA)).toBeInTheDocument()
    expect(api.saveLocalModelsConfig).toHaveBeenCalledWith(URL_OLLAMA, undefined)
    expect(api.getLocalModelsConfig).toHaveBeenCalledTimes(2)
    expect(onConfigured).toHaveBeenCalledTimes(1)
    expect(screen.getByText('Local model server configured')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('button', {name: 'Edit'})).toHaveFocus())
  })

  it('shows the server message for INVALID_BASE_URL in an alert and stays on the form', async () => {
    api.saveLocalModelsConfig.mockRejectedValue(
      new ApiError('The base URL must point at this computer', 400, 'INVALID_BASE_URL'),
    )
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: 'http://example.com/v1'}})
    fireEvent.click(saveButton())
    expect(await screen.findByRole('alert')).toHaveTextContent('The base URL must point at this computer')
    expect(baseUrlField()).toBeInTheDocument()
  })

  it('falls back to generic copy for a non-API save failure', async () => {
    api.saveLocalModelsConfig.mockRejectedValue(new Error('boom'))
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    fireEvent.click(saveButton())
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to save the local model settings')
  })

  it('renders nothing and tells the host when the server answers LOCAL_MODELS_UNAVAILABLE on load', async () => {
    const onUnavailable = vi.fn()
    api.getLocalModelsConfig.mockRejectedValue(unavailable())
    const {container} = render(<LocalModelsSection onUnavailable={onUnavailable} />)
    await waitFor(() => expect(onUnavailable).toHaveBeenCalledTimes(1))
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when a save answers LOCAL_MODELS_UNAVAILABLE', async () => {
    const onUnavailable = vi.fn()
    api.saveLocalModelsConfig.mockRejectedValue(unavailable())
    const {container} = render(<LocalModelsSection onUnavailable={onUnavailable} />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    fireEvent.click(saveButton())
    await waitFor(() => expect(container).toBeEmptyDOMElement())
    expect(onUnavailable).toHaveBeenCalledTimes(1)
  })

  it('offers an optional context window field with the 8192 placeholder and a hint', async () => {
    render(<LocalModelsSection />)
    await screen.findByRole('textbox', {name: 'Base URL'})
    expect(windowField()).toHaveAttribute('placeholder', '8192')
    expect(windowField()).toHaveAttribute('inputmode', 'numeric')
    expect(windowField()).toHaveAccessibleDescription(/Leave blank for 8192/)
  })

  it('saves the context window as a number', async () => {
    api.getLocalModelsConfig.mockResolvedValueOnce(empty).mockResolvedValueOnce({...configured, contextWindow: 32768})
    api.saveLocalModelsConfig.mockResolvedValue(undefined)
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    fireEvent.change(windowField(), {target: {value: ' 32768 '}})
    fireEvent.click(saveButton())
    expect(await screen.findByText(URL_OLLAMA)).toBeInTheDocument()
    expect(api.saveLocalModelsConfig).toHaveBeenCalledWith(URL_OLLAMA, 32768)
  })

  it.each(['512', '2000001', '8192.5', '-8192', '8k', '1e4'])(
    'flags the context window %s after blur-sm and keeps Save disabled',
    async value => {
      render(<LocalModelsSection />)
      fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
      fireEvent.change(windowField(), {target: {value}})
      expect(screen.queryByText(WINDOW_INVALID)).not.toBeInTheDocument()
      expect(saveButton()).toBeDisabled()
      fireEvent.blur(windowField())
      expect(windowField()).toHaveAccessibleDescription(WINDOW_INVALID)
      expect(windowField()).toHaveAttribute('aria-invalid', 'true')
      expect(saveButton()).toBeDisabled()
    },
  )

  it('accepts the bounds of the context window range', async () => {
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    for (const value of ['1024', '2000000', '']) {
      fireEvent.change(windowField(), {target: {value}})
      fireEvent.blur(windowField())
      expect(screen.queryByText(WINDOW_INVALID)).not.toBeInTheDocument()
      expect(saveButton()).toBeEnabled()
    }
  })

  it('shows the server message for INVALID_CONTEXT_WINDOW in an alert and stays on the form', async () => {
    api.saveLocalModelsConfig.mockRejectedValue(
      new ApiError('the context window must be a whole number of tokens', 400, 'INVALID_CONTEXT_WINDOW'),
    )
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    fireEvent.change(windowField(), {target: {value: '4096'}})
    fireEvent.click(saveButton())
    expect(await screen.findByRole('alert')).toHaveTextContent('the context window must be a whole number of tokens')
    expect(windowField()).toHaveValue('4096')
  })

  it('shows an error row with Retry, never the empty form, when the load fails', async () => {
    api.getLocalModelsConfig.mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce(configured)
    render(<LocalModelsSection />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load the local model configuration')
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByText(URL_OLLAMA)).toBeInTheDocument()
  })

  it('says the change was saved when the post-save reload fails, with a Retry', async () => {
    api.getLocalModelsConfig
      .mockResolvedValueOnce(empty)
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce(configured)
    api.saveLocalModelsConfig.mockResolvedValue(undefined)
    render(<LocalModelsSection />)
    fireEvent.change(await screen.findByRole('textbox', {name: 'Base URL'}), {target: {value: URL_OLLAMA}})
    fireEvent.click(saveButton())
    expect(await screen.findByRole('status')).toHaveTextContent(
      "Your change was saved, but the configuration couldn't be reloaded.",
    )
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByText(URL_OLLAMA)).toBeInTheDocument()
  })

  describe('when configured', () => {
    beforeEach(() => {
      api.getLocalModelsConfig.mockResolvedValue(configured)
    })

    it('shows the saved base URL with Edit, Remove and Test', async () => {
      render(<LocalModelsSection />)
      expect(await screen.findByText(URL_OLLAMA)).toBeInTheDocument()
      expect(screen.getByText('Context window')).toBeInTheDocument()
      expect(screen.getByText(formatCount(8192))).toBeInTheDocument()
      expect(screen.getByRole('button', {name: 'Edit'})).toBeInTheDocument()
      expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
      expect(screen.getByRole('button', {name: 'Test'})).toBeInTheDocument()
    })

    it('Edit prefills the URL and focuses the field; Cancel with no change skips the confirm and refocuses Edit', async () => {
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
      await waitFor(() => expect(baseUrlField()).toHaveFocus())
      expect(baseUrlField()).toHaveValue(URL_OLLAMA)
      fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
      await waitFor(() => expect(screen.getByRole('button', {name: 'Edit'})).toHaveFocus())
      expect(confirmMock).not.toHaveBeenCalled()
    })

    it('shows the saved context window in the summary and prefills it on Edit', async () => {
      api.getLocalModelsConfig.mockResolvedValue({...configured, contextWindow: 32768})
      render(<LocalModelsSection />)
      expect(await screen.findByText(formatCount(32768))).toBeInTheDocument()
      fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
      expect(windowField()).toHaveValue('32768')
    })

    it('Cancel after a context window change asks before discarding', async () => {
      confirmMock.mockResolvedValue(false)
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
      fireEvent.change(windowField(), {target: {value: '16384'}})
      fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
      await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
      expect(windowField()).toHaveValue('16384')
    })

    it('Cancel after a change asks before discarding and stays in the form when declined', async () => {
      confirmMock.mockResolvedValue(false)
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Edit'}))
      fireEvent.change(baseUrlField(), {target: {value: 'http://127.0.0.1:1234/v1'}})
      fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
      await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
      expect(baseUrlField()).toHaveValue('http://127.0.0.1:1234/v1')
    })

    it('Test reports latency in a status region', async () => {
      api.testProviderConnection.mockResolvedValue({ok: true, latencyMs: 42})
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
      await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent(/42/))
      expect(api.testProviderConnection).toHaveBeenCalledWith('local')
    })

    it('Test failure shows Invalid plus the server reason in an alert', async () => {
      api.testProviderConnection.mockResolvedValue({ok: false, latencyMs: 0, error: 'connection refused'})
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
      const alert = await screen.findByRole('alert')
      expect(alert).toHaveTextContent('Invalid')
      expect(alert).toHaveTextContent('connection refused')
    })

    it('Test that throws falls back to the error message', async () => {
      api.testProviderConnection.mockRejectedValue(new Error('network down'))
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Test'}))
      expect(await screen.findByRole('alert')).toHaveTextContent('network down')
    })

    it('Remove deletes the key for provider local, reloads, notifies and focuses the URL field', async () => {
      const onConfigured = vi.fn()
      api.deleteApiKey.mockResolvedValue(undefined)
      api.getLocalModelsConfig.mockResolvedValueOnce(configured).mockResolvedValueOnce(empty)
      render(<LocalModelsSection onConfigured={onConfigured} />)
      fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
      await waitFor(() => expect(baseUrlField()).toHaveFocus())
      expect(api.deleteApiKey).toHaveBeenCalledWith('local')
      expect(onConfigured).toHaveBeenCalledTimes(1)
    })

    it('Remove does nothing when the confirm is declined', async () => {
      confirmMock.mockResolvedValue(false)
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
      await waitFor(() => expect(confirmMock).toHaveBeenCalledTimes(1))
      expect(api.deleteApiKey).not.toHaveBeenCalled()
    })

    it('a failed Remove shows an alert and keeps the summary', async () => {
      api.deleteApiKey.mockRejectedValue(new Error('boom'))
      render(<LocalModelsSection />)
      fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
      expect(await screen.findByRole('alert')).toHaveTextContent('Failed to remove the local model configuration')
      expect(screen.getByText(URL_OLLAMA)).toBeInTheDocument()
    })
  })
})

describe('providersApi.saveLocalModelsConfig', () => {
  it('sends contextWindow only when it is set', async () => {
    const {providersApi: real} = await vi.importActual<typeof import('@/api/providers')>('@/api/providers')
    const send = vi.mocked(request)
    send.mockResolvedValue(undefined)
    await real.saveLocalModelsConfig(URL_OLLAMA)
    await real.saveLocalModelsConfig(URL_OLLAMA, 16384)
    expect(send).toHaveBeenNthCalledWith(1, '/api/providers/local/config', {body: {baseUrl: URL_OLLAMA}})
    expect(send).toHaveBeenNthCalledWith(2, '/api/providers/local/config', {
      body: {baseUrl: URL_OLLAMA, contextWindow: 16384},
    })
    expect(Object.keys(send.mock.calls[0]![1]?.body as object)).toEqual(['baseUrl'])
  })
})
