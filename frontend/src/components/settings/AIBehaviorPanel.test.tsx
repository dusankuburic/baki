import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent, within, act} from '@testing-library/react'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {...actual.providersApi, listProviders: vi.fn().mockResolvedValue([]), getProviderCatalog: vi.fn()},
  }
})

import {providersApi} from '@/api'
import AIBehaviorPanel from './AIBehaviorPanel'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStore} from '@/stores/chatStore'
import {useSystemStore} from '@/stores/systemStore'
import type {AppInfo, ProviderSpec} from '@/types'

const initialSettings = useSettingsStore.getState().settings

const systemInfo = (sessionAnalytics: boolean): AppInfo => ({
  version: 'dev',
  platform: 'linux',
  arch: 'amd64',
  buildDate: '',
  gitCommit: '',
  capabilities: {sessionAnalytics},
})

beforeEach(() => {
  useSettingsStore.setState({settings: initialSettings})
  useChatStore.setState({selectedProvider: 'claude'})
  useSystemStore.setState({info: null, isLoaded: false})
  vi.mocked(providersApi.listProviders).mockResolvedValue([])
  // An older server: no catalogue, so the bundle's own lists apply.
  vi.mocked(providersApi.getProviderCatalog).mockRejectedValue(new Error('404'))
})

describe('AIBehaviorPanel', () => {
  it('stores custom instructions as they are typed — no blur-sm needed', () => {
    const {unmount} = render(<AIBehaviorPanel />)
    fireEvent.change(screen.getByRole('textbox', {name: 'Custom Instructions'}), {
      target: {value: 'Always answer in Serbian.'},
    })
    // Closing Settings with Escape unmounts the focused field without a blur.
    unmount()
    expect(useSettingsStore.getState().settings.ai.systemPromptSuffix).toBe('Always answer in Serbian.')
  })

  it('switches the chat to the chosen active assistant right away', () => {
    render(<AIBehaviorPanel />)
    fireEvent.click(screen.getAllByRole('radio', {name: 'Gemini'})[0]!)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('gemini')
    // The chat used to keep its start-up provider until a reload.
    expect(useChatStore.getState().selectedProvider).toBe('gemini')
  })

  it('offers "Default for new chats" as free text, but points Azure AI Foundry to Make default', () => {
    render(<AIBehaviorPanel />)
    // The default assistant (claude) offers the free-text model override.
    const field = screen.getByRole('textbox', {name: 'Default for new chats'})
    expect(field).toHaveAccessibleDescription(/Each chat keeps the model it was switched to/)
    // Each chat remembers its own model; the assistant is where new ones start.
    expect(screen.getByText(/Each chat remembers its own model/)).toBeInTheDocument()

    // Azure AI Foundry models are registry entries: the default is chosen
    // with Make default in AI Accounts, never typed.
    fireEvent.click(screen.getAllByRole('radio', {name: 'Azure AI Foundry'})[0]!)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('azure-foundry')
    expect(screen.queryByRole('textbox', {name: 'Default for new chats'})).toBeNull()
    expect(screen.getByText('Default for new chats')).toBeInTheDocument()
    expect(screen.getByText(/choose "Make default" on a model under AI Accounts/)).toBeInTheDocument()

    // And it comes back for a regular provider.
    fireEvent.click(screen.getAllByRole('radio', {name: 'Claude'})[0]!)
    expect(screen.getByRole('textbox', {name: 'Default for new chats'})).toBeInTheDocument()
  })
})

// The local model provider exists only on a local backend: a cloud server
// refuses it (LOCAL_MODELS_UNAVAILABLE), so it is not offered as an assistant
// there, and it is never an embedding provider (it cannot embed).
describe('AIBehaviorPanel local model gating', () => {
  const activeAssistant = () => screen.getAllByRole('radiogroup')[0]!
  const embeddingAssistant = () => screen.getByRole('radiogroup', {name: 'Embedding Assistant'})

  it('offers the local model as an assistant on a local backend', () => {
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    render(<AIBehaviorPanel />)
    fireEvent.click(within(activeAssistant()).getByRole('radio', {name: 'Local model'}))
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('local')
    expect(useChatStore.getState().selectedProvider).toBe('local')
  })

  it('does not offer the local model as an assistant on a cloud backend', () => {
    useSystemStore.setState({info: systemInfo(false), isLoaded: true})
    render(<AIBehaviorPanel />)
    expect(within(activeAssistant()).queryByRole('radio', {name: 'Local model'})).toBeNull()
    // The rest of the catalogue is still there.
    expect(within(activeAssistant()).getByRole('radio', {name: 'Azure AI Foundry'})).toBeInTheDocument()
  })

  it('falls back to the desktop shell until the system info loads', () => {
    render(<AIBehaviorPanel />)
    expect(within(activeAssistant()).queryByRole('radio', {name: 'Local model'})).toBeNull()
    ;(window as unknown as Record<string, unknown>).__TAURI_INTERNALS__ = {}
    try {
      const {unmount} = render(<AIBehaviorPanel />)
      expect(screen.getAllByRole('radio', {name: 'Local model'})).toHaveLength(1)
      unmount()
    } finally {
      delete (window as unknown as Record<string, unknown>).__TAURI_INTERNALS__
    }
  })

  it('never lists the local model as an embedding assistant', () => {
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    render(<AIBehaviorPanel />)
    expect(within(embeddingAssistant()).queryByRole('radio', {name: 'Local model'})).toBeNull()
    expect(within(embeddingAssistant()).getByRole('radio', {name: 'OpenAI'})).toBeInTheDocument()
  })
})

describe('AIBehaviorPanel embedding model for Azure AI Foundry', () => {
  it('replaces the override with a pointer to "Use for knowledge base" in AI Accounts', () => {
    render(<AIBehaviorPanel />)
    expect(screen.getByRole('textbox', {name: 'Embedding Model'})).toBeInTheDocument()
    // Index 1 = the EMBEDDING assistant control (0 is the active assistant).
    fireEvent.click(screen.getAllByRole('radio', {name: 'Azure AI Foundry'})[1]!)
    expect(useSettingsStore.getState().settings.ai.embeddingProvider).toBe('azure-foundry')
    // The knowledge-base model is a registry designation there, not free text.
    expect(screen.queryByRole('textbox', {name: 'Embedding Model'})).toBeNull()
    expect(screen.getByText(/choose "Use for knowledge base" in its menu/)).toBeInTheDocument()
    expect(screen.queryByText(/Override the embedding model name/i)).toBeNull()

    // Back to OpenAI: the field and the generic hint return.
    fireEvent.click(screen.getAllByRole('radio', {name: 'OpenAI'})[1]!)
    expect(useSettingsStore.getState().settings.ai.embeddingProvider).toBe('openai')
    expect(screen.getByRole('textbox', {name: 'Embedding Model'})).toBeInTheDocument()
    expect(screen.getByText(/Override the embedding model name/i)).toBeInTheDocument()
    expect(screen.queryByText(/Use for knowledge base/)).toBeNull()
  })
})

describe('AIBehaviorPanel max tokens reasoning-floor hint', () => {
  it('shows the reasoning-floor hint under Max Tokens', () => {
    render(<AIBehaviorPanel />)
    expect(screen.getByText(/always sent at least 16[.,]384 tokens/)).toBeInTheDocument()
    expect(screen.getByText(/whatever is set here/)).toBeInTheDocument()
  })
})

describe('AIBehaviorPanel reasoning effort', () => {
  it('renders the always-visible select and stores the choice per provider', () => {
    render(<AIBehaviorPanel />)
    const select = screen.getByLabelText('Reasoning Effort')
    // 'Provider default' first; applies to reasoning models only via the hint.
    expect(screen.getByText('Provider default')).toBeInTheDocument()
    expect(screen.getByText(/reasoning models only/i)).toBeInTheDocument()
    expect((select as HTMLSelectElement).value).toBe('')

    fireEvent.change(select, {target: {value: 'high'}})
    expect(useSettingsStore.getState().settings.ai.providers.claude.reasoningEffort).toBe('high')

    // The choice is per provider: switching keeps it stored for the first.
    fireEvent.click(screen.getAllByRole('radio', {name: 'Gemini'})[0]!)
    expect((screen.getByLabelText('Reasoning Effort') as HTMLSelectElement).value).toBe('')
    expect(useSettingsStore.getState().settings.ai.providers.claude.reasoningEffort).toBe('high')
  })
})

// A provider the server names but this bundle has no catalogue entry for
// (Phase 4b: provider ids are open on the wire in both directions).
describe('AIBehaviorPanel providers the bundle does not know', () => {
  const activeAssistant = () => screen.getAllByRole('radiogroup')[0]!

  // R1: a settings file written by a newer build (or by hand) names an
  // assistant this bundle cannot label. It is still the saved choice: the
  // control shows it selected instead of nothing, and the Advanced block
  // tunes it from the defaults instead of vanishing.
  it('an unknown saved activeProvider still selects a segment', () => {
    useSettingsStore.setState({
      settings: {...initialSettings, ai: {...initialSettings.ai, activeProvider: 'mistral'}},
    })
    render(<AIBehaviorPanel />)
    expect(within(activeAssistant()).getByRole('radio', {name: 'mistral'})).toHaveAttribute('aria-checked', 'true')
    expect(within(activeAssistant()).getByRole('radio', {name: 'Claude'})).toHaveAttribute('aria-checked', 'false')
    expect(screen.getByRole('textbox', {name: 'Default for new chats'})).toHaveValue('')
    expect(screen.getByLabelText('Reasoning Effort')).toBeInTheDocument()
  })

  it('offers an active provider the catalogue lists but the bundle does not know, labelled by its server name', async () => {
    vi.mocked(providersApi.getProviderCatalog).mockResolvedValue([
      {
        id: 'mistral',
        name: 'Mistral',
        configured: true,
        authType: 'api_key',
        modes: ['cloud'],
        caps: ['chat'],
        credential: {kind: 'api_key', fields: []},
        endpoints: {},
        pricing: 'catalog',
      },
    ])
    render(<AIBehaviorPanel />)
    const seg = await within(activeAssistant()).findByRole('radio', {name: 'Mistral'})
    fireEvent.click(seg)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('mistral')
    expect(useChatStore.getState().selectedProvider).toBe('mistral')
    expect(seg).toHaveAttribute('aria-checked', 'true')
    // The catalogue's own entries keep their labels, once each.
    expect(within(activeAssistant()).getAllByRole('radio', {name: 'Claude'})).toHaveLength(1)
  })

  it('keeps the control usable when the catalogue cannot be read', async () => {
    vi.mocked(providersApi.getProviderCatalog).mockRejectedValue(new Error('offline'))
    render(<AIBehaviorPanel />)
    fireEvent.click(within(activeAssistant()).getByRole('radio', {name: 'Gemini'}))
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('gemini')
    await Promise.resolve()
    expect(within(activeAssistant()).getByRole('radio', {name: 'Gemini'})).toHaveAttribute('aria-checked', 'true')
  })
})

// The catalogue (GET /api/providers/catalog) says which providers embed; the
// literal list is only the fallback for a server without the route.
describe('AIBehaviorPanel embedding providers from the catalogue', () => {
  const embeddingAssistant = () => screen.getByRole('radiogroup', {name: 'Embedding Assistant'})
  const spec = (id: string, name: string, caps: string[]): ProviderSpec => ({
    id,
    name,
    authType: 'api_key',
    modes: ['cloud'],
    caps,
    credential: {kind: 'api_key', fields: []},
    endpoints: {},
    configured: false,
    pricing: 'catalog',
  })

  it('the embedding options follow the catalogue caps, not a literal list', async () => {
    vi.mocked(providersApi.getProviderCatalog).mockResolvedValue([
      spec('openai', 'OpenAI', ['chat']),
      spec('mistral', 'Mistral', ['chat', 'embeddings']),
      spec('gemini', 'Gemini', ['embeddings']),
    ])
    render(<AIBehaviorPanel />)
    const mistral = await within(embeddingAssistant()).findByRole('radio', {name: 'Mistral'})
    expect(
      within(embeddingAssistant())
        .getAllByRole('radio')
        .map(r => r.textContent),
    ).toEqual(['Mistral', 'Gemini'])
    fireEvent.click(mistral)
    expect(useSettingsStore.getState().settings.ai.embeddingProvider).toBe('mistral')
  })

  it('keeps the literal list without a catalogue (an older server)', async () => {
    vi.mocked(providersApi.getProviderCatalog).mockRejectedValue(new Error('404'))
    render(<AIBehaviorPanel />)
    await act(async () => {})
    expect(
      within(embeddingAssistant())
        .getAllByRole('radio')
        .map(r => r.textContent),
    ).toEqual(['OpenAI', 'Gemini', 'GLM', 'GitHub Models', 'Azure AI Foundry'])
  })

  it('still selects a saved embedding provider the catalogue no longer lists', async () => {
    vi.mocked(providersApi.getProviderCatalog).mockResolvedValue([spec('openai', 'OpenAI', ['chat', 'embeddings'])])
    useSettingsStore.setState({
      settings: {...initialSettings, ai: {...initialSettings.ai, embeddingProvider: 'voyage'}},
    })
    render(<AIBehaviorPanel />)
    expect(await within(embeddingAssistant()).findByRole('radio', {name: 'OpenAI'})).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(within(embeddingAssistant()).getByRole('radio', {name: 'voyage'})).toHaveAttribute('aria-checked', 'true')
  })
})
