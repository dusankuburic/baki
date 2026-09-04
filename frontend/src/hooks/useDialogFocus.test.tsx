import {describe, it, expect, vi} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useDialogFocus} from './useDialogFocus'
import {useRef} from 'react'

describe('useDialogFocus', () => {
  it('Esc calls onClose when open (closeOnEsc default true)', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, containerRef: r})
    })
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('closeOnEsc=false leaves Esc unhandled', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, closeOnEsc: false, containerRef: r})
    })
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).not.toHaveBeenCalled()
  })

  it('listener is removed on unmount', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    const {unmount} = renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, containerRef: r})
    })
    unmount()
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).not.toHaveBeenCalled()
  })
})

describe('useDialogFocus focus restoration', () => {
  async function flushFrame() {
    await act(async () => {
      await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
    })
  }

  function mountOpenDialog() {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()

    const container = document.createElement('div')
    const inner = document.createElement('button')
    container.appendChild(inner)
    document.body.appendChild(container)

    return {trigger, container, inner}
  }

  // Regression: restore used to live in an `else` branch on isOpen, so the
  // `{open && <Dialog isOpen … />}` pattern — which unmounts while isOpen is
  // still true — dropped focus onto <body>. WCAG 2.4.3.
  it('restores focus to the trigger when the dialog unmounts while still open', async () => {
    const {trigger, container, inner} = mountOpenDialog()

    const {unmount} = renderHook(() => {
      const r = useRef<HTMLElement | null>(null)
      r.current = container
      useDialogFocus({isOpen: true, onClose: vi.fn(), containerRef: r})
    })
    await flushFrame()
    expect(document.activeElement).toBe(inner)

    unmount()

    expect(document.activeElement).toBe(trigger)
    trigger.remove()
    container.remove()
  })

  // The focus lifecycle must not be re-run by an onClose identity change, or a
  // parent re-render yanks focus back out of an open dialog mid-interaction.
  it('keeps focus inside the dialog when onClose identity changes', async () => {
    const {trigger, container, inner} = mountOpenDialog()

    const {rerender} = renderHook(
      ({onClose}: {onClose: () => void}) => {
        const r = useRef<HTMLElement | null>(null)
        r.current = container
        useDialogFocus({isOpen: true, onClose, containerRef: r})
      },
      {initialProps: {onClose: () => {}}},
    )
    await flushFrame()
    expect(document.activeElement).toBe(inner)

    rerender({onClose: () => {}})

    expect(document.activeElement).toBe(inner)
    trigger.remove()
    container.remove()
  })
})
