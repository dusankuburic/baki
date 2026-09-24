import {describe, it, expect, vi} from 'vitest'
import {render, screen, act, fireEvent} from '@testing-library/react'
import {createRef} from 'react'
import ExpandedChatInput from './ExpandedChatInput'

type Props = React.ComponentProps<typeof ExpandedChatInput>

function renderEditor(overrides: Partial<Props> = {}) {
  const props: Props = {
    value: 'draft',
    textareaRef: createRef<HTMLTextAreaElement>(),
    onChange: vi.fn(),
    onKeyDown: vi.fn(),
    onSend: vi.fn(),
    onClose: vi.fn(),
    excludeContext: false,
    onExcludeContextChange: vi.fn(),
    canSend: true,
    menus: null,
    onInsertTrigger: vi.fn(),
    ...overrides,
  }
  render(<ExpandedChatInput {...props} />)
  return props
}

describe('ExpandedChatInput', () => {
  // The expanded editor is for writing: it must open with the caret in its
  // textarea. The Modal's own initial focus (its title bar's Close button,
  // applied a frame later) used to win, so typing went nowhere and the first
  // Space or Enter closed the editor.
  it('opens with focus in the textarea, not on Close', async () => {
    vi.useFakeTimers()
    try {
      renderEditor()
      // Let the Modal's deferred (requestAnimationFrame) focus run.
      await act(async () => {
        vi.advanceTimersByTime(50)
      })
      expect(document.activeElement).toBe(screen.getByRole('textbox'))
    } finally {
      vi.useRealTimers()
    }
  })

  it('greys out Send while there is nothing to send and only offers Preview when the composer can', () => {
    renderEditor({canSend: false})
    expect(screen.getByRole('button', {name: 'Send'})).toBeDisabled()
    expect(screen.queryByRole('button', {name: 'Preview'})).not.toBeInTheDocument()
  })

  it('sends and previews through the footer buttons', () => {
    const props = renderEditor({onPreview: vi.fn()})
    fireEvent.click(screen.getByRole('button', {name: 'Preview'}))
    expect(props.onPreview).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', {name: 'Send'}))
    expect(props.onSend).toHaveBeenCalledTimes(1)
  })

  it('inserts @ and / through the toolbar chips', () => {
    const props = renderEditor()
    fireEvent.click(screen.getByRole('button', {name: 'Files'}))
    expect(props.onInsertTrigger).toHaveBeenLastCalledWith('@')
    fireEvent.click(screen.getByRole('button', {name: 'Commands'}))
    expect(props.onInsertTrigger).toHaveBeenLastCalledWith('/')
  })

  it('toggles the file-context exclusion from a labelled checkbox', () => {
    const props = renderEditor()
    fireEvent.click(screen.getByRole('checkbox', {name: 'Exclude current file context'}))
    expect(props.onExcludeContextChange).toHaveBeenCalledWith(true)
  })
})
