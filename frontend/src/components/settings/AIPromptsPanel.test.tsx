import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import AIPromptsPanel from './AIPromptsPanel'
import {useSettingsStore} from '@/stores/settingsStore'

const initialSettings = useSettingsStore.getState().settings

function seed() {
  useSettingsStore.setState({
    settings: {
      ...initialSettings,
      ai: {
        ...initialSettings.ai,
        prompts: {flow: ['flow A', 'flow B'], block: ['block A', 'block B'], finding: [], blockWithFindings: []},
      },
    },
  })
}

function payload(list: string, from: number) {
  const data = JSON.stringify({list, from})
  return {getData: () => data, setData: () => {}, dropEffect: 'move', effectAllowed: 'move'}
}

beforeEach(seed)

describe('AIPromptsPanel reordering', () => {
  it('reorders within a list', () => {
    render(<AIPromptsPanel />)
    fireEvent.drop(screen.getByDisplayValue('block A'), {dataTransfer: payload('block', 1)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['block B', 'block A'])
  })

  it('ignores a row dragged in from another list', () => {
    render(<AIPromptsPanel />)
    fireEvent.drop(screen.getByDisplayValue('block A'), {dataTransfer: payload('flow', 1)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['block A', 'block B'])
  })

  it('ignores an out-of-range index instead of inserting an empty prompt', () => {
    render(<AIPromptsPanel />)
    fireEvent.drop(screen.getByDisplayValue('block A'), {dataTransfer: payload('block', 7)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['block A', 'block B'])
  })
})

describe('AIPromptsPanel drop position', () => {
  it('drops a row dragged downward where the indicator shows, not one row lower', () => {
    useSettingsStore.setState({
      settings: {
        ...initialSettings,
        ai: {
          ...initialSettings.ai,
          prompts: {flow: [], block: ['A', 'B', 'C'], finding: [], blockWithFindings: []},
        },
      },
    })
    render(<AIPromptsPanel />)
    fireEvent.drop(screen.getByDisplayValue('C'), {dataTransfer: payload('block', 0)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['B', 'A', 'C'])
  })
})

describe('AIPromptsPanel row editing', () => {
  it('never saves a row edited to blank — the store keeps the last value and the row says why', () => {
    render(<AIPromptsPanel />)
    const row = screen.getByDisplayValue('flow A')
    fireEvent.change(row, {target: {value: '   '}})
    expect(useSettingsStore.getState().settings.ai.prompts?.flow).toEqual(['flow A', 'flow B'])
    expect(screen.getByText('A prompt cannot be empty')).toBeInTheDocument()
    expect(row).toHaveAttribute('aria-invalid', 'true')

    fireEvent.change(row, {target: {value: 'flow A2'}})
    expect(useSettingsStore.getState().settings.ai.prompts?.flow).toEqual(['flow A2', 'flow B'])
    expect(screen.queryByText('A prompt cannot be empty')).not.toBeInTheDocument()
  })

  it('re-seeds a row from the store after a reorder, discarding a blank draft', () => {
    render(<AIPromptsPanel />)
    const row = screen.getByDisplayValue('block A')
    fireEvent.change(row, {target: {value: ''}})
    fireEvent.drop(row, {dataTransfer: payload('block', 1)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['block B', 'block A'])
    expect(screen.queryByText('A prompt cannot be empty')).not.toBeInTheDocument()
    expect(screen.getByDisplayValue('block B')).toBeInTheDocument()
  })
})
