import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, act, fireEvent} from '@testing-library/react'
import {useEffect} from 'react'
import ChatPanelShell, {useChatPanel} from './ChatPanelShell'
import {useUIStore} from '@/stores/uiStore'

// jsdom has no matchMedia; focus mode is desktop-only.
vi.mock('@/hooks/useMediaQuery', () => ({useIsDesktop: () => true, useMediaQuery: () => true}))

// Entering or leaving focus mode must not remount the panel: it owns scroll
// position, search, the composer draft — and its unmount used to cancel every
// reply in flight.

let mounts = 0
let unmounts = 0
let toggle: (() => void) | undefined

function Probe() {
  const panel = useChatPanel()
  useEffect(() => {
    toggle = panel?.toggleFocus
  })
  useEffect(() => {
    mounts++
    return () => {
      unmounts++
    }
  }, [])
  return (
    <div>
      <div data-testid="probe">{panel?.isFocused ? 'focused' : 'docked'}</div>
      <button>Header action</button>
      <textarea data-chat-composer="" aria-label="composer" />
    </div>
  )
}

async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

beforeEach(() => {
  mounts = 0
  unmounts = 0
  useUIStore.setState({chatFocus: false, inspectorTab: 'details', inspectorCollapsed: false})
})

describe('ChatPanelShell', () => {
  it('keeps its content mounted across focus mode toggles', () => {
    const {getByTestId} = render(
      <ChatPanelShell>
        <Probe />
      </ChatPanelShell>,
    )
    expect(getByTestId('probe')).toHaveTextContent('docked')

    act(() => toggle!())
    expect(getByTestId('probe')).toHaveTextContent('focused')
    act(() => toggle!())
    expect(getByTestId('probe')).toHaveTextContent('docked')

    expect(mounts).toBe(1)
    expect(unmounts).toBe(0)
  })

  it('is a dialog only while focused, and the backdrop leaves focus mode', () => {
    render(
      <ChatPanelShell>
        <Probe />
      </ChatPanelShell>,
    )
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

    act(() => useUIStore.getState().setChatFocus(true))
    expect(screen.getByRole('dialog', {name: 'AI Chat'})).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Exit focus mode'}))
    expect(useUIStore.getState().chatFocus).toBe(false)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('lands focus in the composer on entering and keeps it there on leaving', async () => {
    render(
      <>
        <button>Outside</button>
        <ChatPanelShell>
          <Probe />
        </ChatPanelShell>
      </>,
    )
    const outside = screen.getByRole('button', {name: 'Outside'})
    outside.focus()

    act(() => useUIStore.getState().setChatFocus(true))
    await flushFrame()
    const composer = screen.getByLabelText('composer')
    expect(document.activeElement).toBe(composer)

    // The panel is the same tree docked: the user's place in it is kept,
    // rather than jumping back to whatever opened the overlay.
    act(() => useUIStore.getState().setChatFocus(false))
    expect(document.activeElement).toBe(composer)
  })

  it('keeps focus where it already is inside the panel', async () => {
    render(
      <ChatPanelShell>
        <Probe />
      </ChatPanelShell>,
    )
    const header = screen.getByRole('button', {name: 'Header action'})
    header.focus()

    act(() => useUIStore.getState().setChatFocus(true))
    await flushFrame()
    expect(document.activeElement).toBe(header)
  })
})
