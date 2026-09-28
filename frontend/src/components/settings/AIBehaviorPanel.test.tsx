import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import AIBehaviorPanel from './AIBehaviorPanel'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStore} from '@/stores/chatStore'

const initialSettings = useSettingsStore.getState().settings

beforeEach(() => {
  useSettingsStore.setState({settings: initialSettings})
  useChatStore.setState({selectedProvider: 'claude'})
})

describe('AIBehaviorPanel', () => {
  it('stores custom instructions as they are typed — no blur needed', () => {
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
    fireEvent.click(screen.getAllByRole('radio', {name: 'Gemini'})[0])
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('gemini')
    // The chat used to keep its start-up provider until a reload.
    expect(useChatStore.getState().selectedProvider).toBe('gemini')
  })

  it('hides the Default Model ID override for azure-foundry but shows it otherwise', () => {
    render(<AIBehaviorPanel />)
    // The default assistant (claude) offers the free-text model override.
    expect(screen.getByText('Default Model ID')).toBeInTheDocument()

    // Azure Foundry deployments are user-named: a free-text model ID can only
    // pin a non-existent deployment, so the field is hidden there (the store
    // defaults it to '' so the server-reported deployment wins).
    fireEvent.click(screen.getAllByRole('radio', {name: 'Azure AI Foundry'})[0])
    expect(useSettingsStore.getState().settings.ai.activeProvider).toBe('azure-foundry')
    expect(screen.queryByText('Default Model ID')).toBeNull()

    // And it comes back for a regular provider.
    fireEvent.click(screen.getAllByRole('radio', {name: 'Claude'})[0])
    expect(screen.getByText('Default Model ID')).toBeInTheDocument()
  })
})
