import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useFlowStore} from '@/stores/flowStore'

vi.mock('@/api', () => ({flowApi: {}, analysisApi: {}, exportApi: {}}))

import {useAppShortcuts} from './useAppShortcuts'

function press(init: KeyboardEventInit, target: EventTarget = document.body) {
  target.dispatchEvent(new KeyboardEvent('keydown', {bubbles: true, cancelable: true, ...init}))
}

describe('useAppShortcuts navigation history', () => {
  beforeEach(() => {
    renderHook(() =>
      useAppShortcuts({
        openDocument: vi.fn(),
        toggleTheme: vi.fn(),
        toast: {} as never,
        setShortcutsHelpOpen: vi.fn(),
      }),
    )
  })

  it('alt+left / alt+right drive the same back/forward as the toolbar buttons', () => {
    const goBack = vi.fn()
    const goForward = vi.fn()
    useFlowStore.setState({goBack, goForward})

    press({key: 'ArrowLeft', altKey: true})
    expect(goBack).toHaveBeenCalledTimes(1)
    press({key: 'ArrowRight', altKey: true})
    expect(goForward).toHaveBeenCalledTimes(1)
  })

  it('leaves alt+arrow alone inside text fields (word-wise caret movement)', () => {
    const goBack = vi.fn()
    useFlowStore.setState({goBack})
    const input = document.createElement('input')
    document.body.appendChild(input)
    input.focus()
    press({key: 'ArrowLeft', altKey: true}, input)
    expect(goBack).not.toHaveBeenCalled()
    input.remove()
  })
})
