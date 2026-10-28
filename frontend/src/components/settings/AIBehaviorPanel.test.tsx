import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent, within, act} from '@testing-library/react'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {...actual.providersApi, listProviders: vi.fn().mockResolvedValue([]), getProviderCatalog: vi.fn()},
    settingsApi: {...actual.settingsApi, getSettings: vi.fn(), updateSettings: vi.fn()},
  }
})

import {providersApi, settingsApi} from '@/api'
import {ApiError} from '@/api/client'
import {__resetPersistQueueForTest} from '@/stores/settingsStore'
import {EMBEDDING_MODEL_MAX_LENGTH, SYSTEM_PROMPT_SUFFIX_MAX_LENGTH} from '@/lib/constraints'
import AIBehaviorPanel from './AIBehaviorPanel'
import {useSettingsStore} from '@/stores/settingsStore'
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
  useSettingsStore.setState({settings: initialSettings, saveError: null})
  __resetPersistQueueForTest()
  vi.mocked(settingsApi.updateSettings).mockReset().mockResolvedValue(undefined)
  vi.mocked(settingsApi.getSettings).mockResolvedValue(null as never)
  useSystemStore.setState({info: null, isLoaded: false})
  vi.mocked(providersApi.listProviders).mockResolvedValue([])
  vi.mocked(providersApi.getProviderCatalog).mockRejectedValue(new Error('404'))
})

describe('AIBehaviorPanel', () => {
  it('stores custom instructions as they are typed — no blur-sm needed', () => {
    const {unmount} = render(<AIBehaviorPanel />)
    fireEvent.change(screen.getByRole('textbox', {name: 'Custom Instructions'}), {
      target: {value: 'Always answer in Serbian.'},
    })
    unmount()
    expect(useSettingsStore.getState().settings.ai.systemPromptSuffix).toBe('Always answer in Serbian.')
  })

  // R-frontend-6: tools are on by default; the setting turns them off for every chat that has not chosen.
  it('shows tools on by default and stores toolsOffByDefault when switched off', () => {
    render(<AIBehaviorPanel />)
    const toggle = screen.getByRole('switch', {name: 'Use tools by default'})
    expect(toggle).toBeChecked()
    fireEvent.click(toggle)
    expect(useSettingsStore.getState().settings.ai.toolsOffByDefault).toBe(true)
    expect(screen.getByRole('switch', {name: 'Use tools by default'})).not.toBeChecked()
  })

  it('stores the chosen active assistant right away', () => {
    render(<AIBehaviorPanel />)
    fireEvent.click(screen.getAllByRole('radio', {name: 'Gemini'})[0]!)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('gemini')
  })

  it('offers "Default for new chats" as free text, but points Azure AI Foundry to Make default', () => {
    render(<AIBehaviorPanel />)
    const field = screen.getByRole('textbox', {name: 'Default for new chats'})
    expect(field).toHaveAccessibleDescription(/Each chat keeps the model it was switched to/)
    expect(screen.getByText(/Each chat remembers its own model/)).toBeInTheDocument()

    fireEvent.click(screen.getAllByRole('radio', {name: 'Azure AI Foundry'})[0]!)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('azure-foundry')
    expect(screen.queryByRole('textbox', {name: 'Default for new chats'})).toBeNull()
    expect(screen.getByText('Default for new chats')).toBeInTheDocument()
    expect(screen.getByText(/choose "Make default" on a model under AI Accounts/)).toBeInTheDocument()

    fireEvent.click(screen.getAllByRole('radio', {name: 'Claude'})[0]!)
    expect(screen.getByRole('textbox', {name: 'Default for new chats'})).toBeInTheDocument()
  })
})

describe('AIBehaviorPanel local model gating', () => {
  const activeAssistant = () => screen.getAllByRole('radiogroup')[0]!
  const embeddingAssistant = () => screen.getByRole('radiogroup', {name: 'Embedding Assistant'})

  it('offers the local model as an assistant on a local backend', () => {
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    render(<AIBehaviorPanel />)
    fireEvent.click(within(activeAssistant()).getByRole('radio', {name: 'Local model'}))
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('local')
  })

  it('does not offer the local model as an assistant on a cloud backend', () => {
    useSystemStore.setState({info: systemInfo(false), isLoaded: true})
    render(<AIBehaviorPanel />)
    expect(within(activeAssistant()).queryByRole('radio', {name: 'Local model'})).toBeNull()
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
    fireEvent.click(screen.getAllByRole('radio', {name: 'Azure AI Foundry'})[1]!)
    expect(useSettingsStore.getState().settings.ai.embeddingProvider).toBe('azure-foundry')
    expect(screen.queryByRole('textbox', {name: 'Embedding Model'})).toBeNull()
    expect(screen.getByText(/choose "Use for knowledge base" in its menu/)).toBeInTheDocument()
    expect(screen.queryByText(/Override the embedding model name/i)).toBeNull()

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
    expect(screen.getByText('Provider default')).toBeInTheDocument()
    expect(screen.getByText(/reasoning models only/i)).toBeInTheDocument()
    expect((select as HTMLSelectElement).value).toBe('')

    fireEvent.change(select, {target: {value: 'high'}})
    expect(useSettingsStore.getState().settings.ai.providers.claude.reasoningEffort).toBe('high')

    fireEvent.click(screen.getAllByRole('radio', {name: 'Gemini'})[0]!)
    expect((screen.getByLabelText('Reasoning Effort') as HTMLSelectElement).value).toBe('')
    expect(useSettingsStore.getState().settings.ai.providers.claude.reasoningEffort).toBe('high')
  })
})

describe('AIBehaviorPanel providers the bundle does not know', () => {
  const activeAssistant = () => screen.getAllByRole('radiogroup')[0]!

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
      },
    ])
    render(<AIBehaviorPanel />)
    const seg = await within(activeAssistant()).findByRole('radio', {name: 'Mistral'})
    fireEvent.click(seg)
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('mistral')
    expect(seg).toHaveAttribute('aria-checked', 'true')
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

describe('AIBehaviorPanel failed saves (R-frontend-10)', () => {
  const activeAssistant = () => screen.getAllByRole('radiogroup')[0]!

  it('returns the active assistant to the stored one and shows the server refusal', async () => {
    vi.useFakeTimers()
    try {
      vi.mocked(settingsApi.updateSettings).mockRejectedValue(
        new ApiError(
          'invalid settings: ai.activeProvider "gemini" is not a provider this server knows',
          400,
          'BAD_REQUEST',
        ),
      )
      // The re-read never answers: the control must come back by the store's own rollback.
      vi.mocked(settingsApi.getSettings).mockReturnValue(new Promise(() => {}))
      render(<AIBehaviorPanel />)
      fireEvent.click(within(activeAssistant()).getByRole('radio', {name: 'Gemini'}))
      expect(within(activeAssistant()).getByRole('radio', {name: 'Gemini'})).toHaveAttribute('aria-checked', 'true')

      await act(async () => {
        await vi.advanceTimersByTimeAsync(1000)
      })

      expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('claude')
      expect(within(activeAssistant()).getByRole('radio', {name: 'Claude'})).toHaveAttribute('aria-checked', 'true')
      expect(screen.getByRole('status')).toHaveTextContent(/is not a provider this server knows/)
    } finally {
      vi.useRealTimers()
    }
  })

  it('posts only the ai section', async () => {
    vi.useFakeTimers()
    try {
      render(<AIBehaviorPanel />)
      fireEvent.click(within(activeAssistant()).getByRole('radio', {name: 'Gemini'}))
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1000)
      })
      expect(settingsApi.updateSettings).toHaveBeenCalledOnce()
      expect(Object.keys(vi.mocked(settingsApi.updateSettings).mock.calls[0]![0]).sort()).toEqual(['ai', 'version'])
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('AIBehaviorPanel caps and the cloud embedding writer (R-config-2)', () => {
  it('stops custom instructions at the server cap', () => {
    render(<AIBehaviorPanel />)
    expect(screen.getByRole('textbox', {name: 'Custom Instructions'})).toHaveAttribute(
      'maxLength',
      String(SYSTEM_PROMPT_SUFFIX_MAX_LENGTH),
    )
  })

  it('stops the embedding model at the server cap', () => {
    render(<AIBehaviorPanel />)
    expect(screen.getByRole('textbox', {name: 'Embedding Model'})).toHaveAttribute(
      'maxLength',
      String(EMBEDDING_MODEL_MAX_LENGTH),
    )
  })

  it('shows the stored custom instructions again after they change without a keystroke', () => {
    render(<AIBehaviorPanel />)
    const field = screen.getByRole('textbox', {name: 'Custom Instructions'})
    fireEvent.change(field, {target: {value: 'refused text'}})
    act(() => {
      useSettingsStore.setState(s => ({
        settings: {...s.settings, ai: {...s.settings.ai, systemPromptSuffix: 'stored'}},
      }))
    })
    expect(field).toHaveValue('stored')
  })

  it('points a cloud member at the administrator for the embedding choice', () => {
    useSystemStore.setState({info: systemInfo(false), isLoaded: true})
    render(<AIBehaviorPanel />)
    expect(screen.queryByRole('radiogroup', {name: 'Embedding Assistant'})).toBeNull()
    expect(screen.queryByRole('textbox', {name: 'Embedding Model'})).toBeNull()
    expect(screen.getByText(/administrator chooses the provider and model/)).toBeInTheDocument()
  })

  it('keeps the embedding choice on a local backend, where this panel writes it', () => {
    useSystemStore.setState({info: systemInfo(true), isLoaded: true})
    render(<AIBehaviorPanel />)
    expect(screen.getByRole('radiogroup', {name: 'Embedding Assistant'})).toBeInTheDocument()
    expect(screen.queryByText(/administrator chooses the provider and model/)).toBeNull()
  })
})
