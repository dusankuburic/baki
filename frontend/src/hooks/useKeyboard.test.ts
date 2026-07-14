import {describe, it, expect, vi} from 'vitest'
import {renderHook} from '@testing-library/react'
import {createRef} from 'react'
import {useKeyboard} from './useKeyboard'

function dispatchKey(init: KeyboardEventInit, target: Element = document.body) {
  const event = new KeyboardEvent('keydown', {bubbles: true, cancelable: true, ...init})
  target.dispatchEvent(event)
  return event
}

describe('useKeyboard', () => {
  it('invokes the handler for a registered global shortcut id', () => {
    const onOpen = vi.fn()
    renderHook(() => useKeyboard({scope: 'global', handlers: {'file.open': onOpen}}))

    dispatchKey({key: 'o', ctrlKey: true})
    expect(onOpen).toHaveBeenCalledTimes(1)
  })

  it('does not invoke a handler outside its declared scope', () => {
    const onCopyName = vi.fn()
    renderHook(() => useKeyboard({scope: 'global', handlers: {'edit.copy.name': onCopyName}}))

    dispatchKey({key: 'c', ctrlKey: true}) // edit.copy.name is scope:'main'
    expect(onCopyName).not.toHaveBeenCalled()
  })

  it('skips non-allowlisted shortcuts when the event target is a text input', () => {
    const onOpen = vi.fn()
    renderHook(() => useKeyboard({scope: 'global', handlers: {'file.open': onOpen}}))

    const input = document.createElement('input')
    document.body.appendChild(input)
    input.focus()
    dispatchKey({key: 'o', ctrlKey: true}, input)

    expect(onOpen).not.toHaveBeenCalled()
    document.body.removeChild(input)
  })

  it('still invokes allowInInputs shortcuts while focused in a text input', () => {
    const onSend = vi.fn()
    renderHook(() => useKeyboard({scope: 'chat', handlers: {'ai.send': onSend}}))

    const textarea = document.createElement('textarea')
    document.body.appendChild(textarea)
    textarea.focus()
    dispatchKey({key: 'Enter', ctrlKey: true}, textarea)

    expect(onSend).toHaveBeenCalledTimes(1)
    document.body.removeChild(textarea)
  })

  it('does nothing when disabled', () => {
    const onOpen = vi.fn()
    renderHook(() => useKeyboard({scope: 'global', handlers: {'file.open': onOpen}, enabled: false}))
    dispatchKey({key: 'o', ctrlKey: true})
    expect(onOpen).not.toHaveBeenCalled()
  })

  it('ignores events outside containerRef when one is provided', () => {
    const onOpen = vi.fn()
    const container = document.createElement('div')
    document.body.appendChild(container)
    const child = document.createElement('button')
    container.appendChild(child)
    const outside = document.createElement('button')
    document.body.appendChild(outside)
    const containerRef = createRef<HTMLElement>() as {current: HTMLElement | null}
    containerRef.current = container

    renderHook(() => useKeyboard({scope: 'global', handlers: {'file.open': onOpen}, containerRef}))

    outside.focus()
    dispatchKey({key: 'o', ctrlKey: true}, outside)
    expect(onOpen).not.toHaveBeenCalled()

    child.focus()
    dispatchKey({key: 'o', ctrlKey: true}, child)
    expect(onOpen).toHaveBeenCalledTimes(1)

    document.body.removeChild(container)
    document.body.removeChild(outside)
  })

  it('always calls the latest handler reference without re-registering the listener', () => {
    const first = vi.fn()
    const second = vi.fn()
    const {rerender} = renderHook(({handler}) => useKeyboard({scope: 'global', handlers: {'file.open': handler}}), {
      initialProps: {handler: first},
    })
    rerender({handler: second})

    dispatchKey({key: 'o', ctrlKey: true})
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('removes the document listener on unmount', () => {
    const onOpen = vi.fn()
    const {unmount} = renderHook(() => useKeyboard({scope: 'global', handlers: {'file.open': onOpen}}))
    unmount()
    dispatchKey({key: 'o', ctrlKey: true})
    expect(onOpen).not.toHaveBeenCalled()
  })
})
