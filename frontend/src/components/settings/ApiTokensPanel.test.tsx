import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import ApiTokensPanel from './ApiTokensPanel'
import {ToastProvider, ConfirmProvider} from '@/components/shared'

const listApiTokens = vi.fn()
const createApiToken = vi.fn()
const revokeApiToken = vi.fn()

vi.mock('@/api/auth', () => ({
  API_TOKEN_SCOPES: ['read', 'write', 'chat', 'admin', 'ci'],
  authApi: {
    listApiTokens: (...a: unknown[]) => listApiTokens(...a),
    createApiToken: (...a: unknown[]) => createApiToken(...a),
    revokeApiToken: (...a: unknown[]) => revokeApiToken(...a),
  },
}))

function renderPanel() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <ApiTokensPanel />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  listApiTokens.mockResolvedValue([{id: 't1', name: 'ci-pipeline', createdAt: '2026-01-01T00:00:00Z'}])
})

describe('ApiTokensPanel', () => {
  it('lists existing tokens', async () => {
    renderPanel()
    expect(await screen.findByText('ci-pipeline')).toBeInTheDocument()
  })

  it('creates a token and reveals the raw secret exactly once', async () => {
    createApiToken.mockResolvedValue({
      id: 't2',
      name: 'new',
      token: 'pad_pat_secret123',
      createdAt: '2026-01-02T00:00:00Z',
    })
    renderPanel()
    await screen.findByText('ci-pipeline')

    fireEvent.change(screen.getByPlaceholderText(/Name/i), {target: {value: 'new'}})
    fireEvent.click(screen.getByRole('button', {name: /Create/i}))

    await waitFor(() => expect(createApiToken).toHaveBeenCalledWith('new', undefined, ['read', 'write']))
    // The raw secret is shown in the one-time reveal banner.
    expect(await screen.findByText('pad_pat_secret123')).toBeInTheDocument()
  })

  it('passes an expiry when days are provided', async () => {
    createApiToken.mockResolvedValue({id: 't3', name: 'temp', token: 'pad_pat_x', createdAt: '2026-01-02T00:00:00Z'})
    renderPanel()
    await screen.findByText('ci-pipeline')

    fireEvent.change(screen.getByPlaceholderText(/Name/i), {target: {value: 'temp'}})
    fireEvent.change(screen.getByPlaceholderText(/Expires/i), {target: {value: '30'}})
    fireEvent.click(screen.getByRole('button', {name: /Create/i}))

    await waitFor(() => expect(createApiToken).toHaveBeenCalledWith('temp', 30, ['read', 'write']))
  })

  it('shows an error when listing fails', async () => {
    listApiTokens.mockRejectedValue(new Error('nope'))
    renderPanel()
    expect(await screen.findByText(/Failed to load tokens/)).toBeInTheDocument()
  })

  it('offers the CI scope, unchecked, with what it grants', async () => {
    renderPanel()
    await screen.findByText('ci-pipeline')
    const ci = screen.getByRole('checkbox', {name: 'CI'})
    expect(ci).not.toBeChecked()
    expect(screen.getByText(/bakicli report -token: analyse the flows you can read/)).toBeInTheDocument()
  })

  it('mints a CI-only token for bakicli report', async () => {
    createApiToken.mockResolvedValue({id: 't4', name: 'runner', token: 'pad_pat_ci', createdAt: '2026-01-02T00:00:00Z'})
    renderPanel()
    await screen.findByText('ci-pipeline')

    fireEvent.click(screen.getByRole('checkbox', {name: 'Read'}))
    fireEvent.click(screen.getByRole('checkbox', {name: 'Write'}))
    fireEvent.click(screen.getByRole('checkbox', {name: 'CI'}))
    fireEvent.change(screen.getByPlaceholderText(/Name/i), {target: {value: 'runner'}})
    fireEvent.click(screen.getByRole('button', {name: /Create/i}))

    await waitFor(() => expect(createApiToken).toHaveBeenCalledWith('runner', undefined, ['ci']))
  })

  it('lists a CI token by its scope and says what an unscoped token cannot do', async () => {
    listApiTokens.mockResolvedValue([{id: 't5', name: 'runner', createdAt: '2026-01-01T00:00:00Z', scopes: ['ci']}])
    renderPanel()
    expect(await screen.findByText('runner')).toBeInTheDocument()
    expect(screen.getByTitle('Scopes')).toHaveTextContent('CI')

    for (const scope of ['Read', 'Write']) fireEvent.click(screen.getByRole('checkbox', {name: scope}))
    expect(screen.getByText(/except run CI \(bakicli report\), which needs the CI scope/)).toBeInTheDocument()
  })
})
