import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import PaneDivider from './PaneDivider'

function renderDivider(overrides: Partial<React.ComponentProps<typeof PaneDivider>> = {}) {
  const props = {
    onDrag: vi.fn(),
    onResizeEnd: vi.fn(),
    onDoubleClick: vi.fn(),
    onJump: vi.fn(),
    label: 'Resize inspector',
    value: 320,
    min: 280,
    max: 840,
    ...overrides,
  }
  render(<PaneDivider {...props} />)
  return {...props, separator: screen.getByRole('separator', {name: 'Resize inspector'})}
}

describe('PaneDivider', () => {
  it('is a labelled, focusable vertical separator that reports its value', () => {
    const {separator} = renderDivider()
    expect(separator).toHaveAttribute('aria-orientation', 'vertical')
    expect(separator).toHaveAttribute('aria-valuenow', '320')
    expect(separator).toHaveAttribute('aria-valuemin', '280')
    expect(separator).toHaveAttribute('aria-valuemax', '840')
    expect(separator).toHaveAttribute('tabindex', '0')
  })

  it('moves 16px per arrow key and commits each step', () => {
    const {separator, onDrag, onResizeEnd} = renderDivider()
    fireEvent.keyDown(separator, {key: 'ArrowLeft'})
    expect(onDrag).toHaveBeenLastCalledWith(-16)
    fireEvent.keyDown(separator, {key: 'ArrowRight'})
    expect(onDrag).toHaveBeenLastCalledWith(16)
    expect(onResizeEnd).toHaveBeenCalledTimes(2)
  })

  it('moves 64px per step with Shift held', () => {
    const {separator, onDrag} = renderDivider()
    fireEvent.keyDown(separator, {key: 'ArrowRight', shiftKey: true})
    expect(onDrag).toHaveBeenLastCalledWith(64)
  })

  it('jumps to the limits with Home and End, and resets on Enter', () => {
    const {separator, onJump, onDoubleClick} = renderDivider()
    fireEvent.keyDown(separator, {key: 'Home'})
    expect(onJump).toHaveBeenLastCalledWith('min')
    fireEvent.keyDown(separator, {key: 'End'})
    expect(onJump).toHaveBeenLastCalledWith('max')
    fireEvent.keyDown(separator, {key: 'Enter'})
    expect(onDoubleClick).toHaveBeenCalledTimes(1)
  })

  it('ignores Home and End when no jump is offered', () => {
    const {separator, onDrag, onResizeEnd} = renderDivider({onJump: undefined})
    fireEvent.keyDown(separator, {key: 'Home'})
    fireEvent.keyDown(separator, {key: 'End'})
    expect(onDrag).not.toHaveBeenCalled()
    expect(onResizeEnd).not.toHaveBeenCalled()
  })

  it('reports pointer deltas and ends a cancelled drag exactly once', () => {
    const {separator, onDrag, onResizeEnd} = renderDivider()
    fireEvent.pointerDown(separator, {clientX: 100, pointerId: 1})
    fireEvent.pointerMove(separator, {clientX: 112, pointerId: 1})
    expect(onDrag).toHaveBeenLastCalledWith(12)
    fireEvent.pointerCancel(separator, {pointerId: 1})
    fireEvent.lostPointerCapture(separator, {pointerId: 1})
    expect(onResizeEnd).toHaveBeenCalledTimes(1)
    fireEvent.pointerMove(separator, {clientX: 200, pointerId: 1})
    expect(onDrag).toHaveBeenCalledTimes(1)
  })
})
