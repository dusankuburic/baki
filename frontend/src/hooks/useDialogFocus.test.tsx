import {describe, it, expect, vi} from 'vitest'
import {renderHook} from '@testing-library/react'
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
