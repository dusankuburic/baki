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
})
