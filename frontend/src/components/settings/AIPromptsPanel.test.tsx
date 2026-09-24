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

// The drag payload a row puts on the DataTransfer when a drag starts.
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
    // A bare index used to reorder the WRONG item of the target list.
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
    // The indicator is C's top border: A lands between B and C.
    fireEvent.drop(screen.getByDisplayValue('C'), {dataTransfer: payload('block', 0)})
    expect(useSettingsStore.getState().settings.ai.prompts?.block).toEqual(['B', 'A', 'C'])
  })
})
