import {describe, it, expect, vi, afterEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import CredentialForm from './CredentialForm'
import {mockFetch} from '@/testing/testHelpers'
import {providerSpec} from '@/testing/fixtures/providerCatalog'
import type {ProviderSpec} from '@/types'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

describe('CredentialForm against the /api/keys/has wire shape', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the key form, not the stored-key row, when the server answers {has: false}', async () => {
    const spy = mockFetch({has: false})
    render(
      <CredentialForm
        spec={providerSpec({id: 'openai', configured: true}) as unknown as ProviderSpec}
        onConfigured={vi.fn()}
      />,
    )
    expect(await screen.findByLabelText('API key')).toBeInTheDocument()
    expect(screen.queryByText(/API key configured/)).toBeNull()
    expect(screen.queryByRole('button', {name: 'Remove'})).toBeNull()
    const [url, init] = spy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://localhost:9999/api/keys/has')
    expect(JSON.parse(init.body as string)).toEqual({provider: 'openai'})
  })

  it('shows the stored-key row when the server answers {has: true}', async () => {
    mockFetch({has: true})
    render(<CredentialForm spec={providerSpec({id: 'openai'}) as unknown as ProviderSpec} onConfigured={vi.fn()} />)
    expect(await screen.findByText(/API key configured/)).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Remove'})).toBeInTheDocument()
  })
})
