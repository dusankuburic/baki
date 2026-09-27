import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import ProvidersPanel from './ProvidersPanel'
import {providersApi} from '@/api'

// Every section does a lookup on mount: the key inputs ask hasApiKey, the
// device-flow buttons ask for the signed-in user, the panel lists providers,
// and the Azure section loads its config document.
vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      listProviders: vi.fn(),
      getGitHubUser: vi.fn(),
      getCopilotUser: vi.fn(),
      hasApiKey: vi.fn(),
      getAzureFoundryConfig: vi.fn(),
    },
  }
})

const api = vi.mocked(providersApi)

const SECTION_NAMES = [
  'Claude',
  'OpenAI',
  'Gemini',
  'xAI (Grok)',
  'GLM (z.ai)',
  'Azure AI Foundry',
  'GitHub Models',
  'GitHub Copilot',
]

beforeEach(() => {
  vi.resetAllMocks()
  api.listProviders.mockResolvedValue([])
  api.getGitHubUser.mockResolvedValue(null)
  api.getCopilotUser.mockResolvedValue(null)
  api.hasApiKey.mockResolvedValue(false)
  api.getAzureFoundryConfig.mockResolvedValue({configured: false})
})

describe('ProvidersPanel', () => {
  it('renders every provider section in order, Azure AI Foundry between GLM and GitHub Models', async () => {
    render(<ProvidersPanel />)
    // The Azure form appears once its own GET resolves.
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toBeInTheDocument()

    const headings = SECTION_NAMES.map(name => screen.getByText(name))
    for (let i = 1; i < headings.length; i++) {
      const follows = headings[i - 1].compareDocumentPosition(headings[i]) & Node.DOCUMENT_POSITION_FOLLOWING
      expect(follows, `${SECTION_NAMES[i]} follows ${SECTION_NAMES[i - 1]}`).toBeTruthy()
    }
    expect(api.getAzureFoundryConfig).toHaveBeenCalledTimes(1)
    expect(api.listProviders).toHaveBeenCalledTimes(1)

    // Let the remaining mount-time lookups settle inside act.
    await act(async () => {})
  })
})
