import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import AIBehaviorPanel from './AIBehaviorPanel'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStore} from '@/stores/chatStore'
import {useSystemStore} from '@/stores/systemStore'
import type {AppInfo} from '@/types'

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
