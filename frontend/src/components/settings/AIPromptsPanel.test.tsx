import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import AIPromptsPanel from './AIPromptsPanel'
import {useSettingsStore} from '@/stores/settingsStore'
import {AI_PROMPT_MAX_LENGTH, AI_PROMPTS_PER_LIST_MAX} from '@/lib/constraints'

const initialSettings = useSettingsStore.getState().settings

const withFlowPrompts = (flow: string[]) =>
  useSettingsStore.setState({
    settings: {...initialSettings, ai: {...initialSettings.ai, prompts: {...initialSettings.ai.prompts, flow}}},
    saveError: null,
  })

beforeEach(() => withFlowPrompts(['Analyze the whole flow']))

// R-config-2: the prompt lists stop at the server's caps, so a list the server would refuse cannot be built here.
describe('AIPromptsPanel caps', () => {
  it('stops a prompt at the server cap', () => {
    render(<AIPromptsPanel />)
    expect(screen.getByRole('textbox', {name: 'Suggested prompt 1 of Flow Context'})).toHaveAttribute(
      'maxLength',
      String(AI_PROMPT_MAX_LENGTH),
    )
    for (const field of screen.getAllByRole('textbox', {name: 'New suggested prompt'})) {
      expect(field).toHaveAttribute('maxLength', String(AI_PROMPT_MAX_LENGTH))
    }
  })

  it('offers no new prompt once a list is full', () => {
    withFlowPrompts(Array.from({length: AI_PROMPTS_PER_LIST_MAX}, (_, i) => `prompt ${i}`))
    render(<AIPromptsPanel />)
    // Four lists, one full: three add fields remain.
    expect(screen.getAllByRole('textbox', {name: 'New suggested prompt'})).toHaveLength(3)
    expect(screen.getByText(`A list holds at most ${AI_PROMPTS_PER_LIST_MAX} prompts.`)).toBeInTheDocument()
  })

  it('shows a refused save of the ai section', () => {
    useSettingsStore.setState({
      saveError: {
        message: 'invalid settings: ai.prompts.flow has 21 prompts, at most 20',
        code: 'BAD_REQUEST',
        sections: ['ai'],
        at: 1,
      },
    })
    render(<AIPromptsPanel />)
    expect(screen.getByRole('status')).toHaveTextContent(/ai\.prompts\.flow has 21 prompts/)
  })
})
