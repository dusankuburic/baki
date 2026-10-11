import {describe, it, expect, vi, afterEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {useRef} from 'react'
import {useAnchoredPosition, type AnchorAlign, type AnchorSide} from './useAnchoredPosition'

type Box = {left: number; top: number; width: number; height: number}

function mockBoxes(anchor: Box, floating: {width: number; height: number}) {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    const box = this.dataset.box === 'floating' ? {left: 0, top: 0, ...floating} : anchor
    return {
      ...box,
      x: box.left,
      y: box.top,
      right: box.left + box.width,
      bottom: box.top + box.height,
      toJSON: () => box,
    } as DOMRect
  })
}

function Harness({open, side, align}: {open: boolean; side?: AnchorSide; align?: AnchorAlign}) {
  const anchorRef = useRef<HTMLButtonElement>(null)
  const floatingRef = useRef<HTMLDivElement>(null)
  const {top, left} = useAnchoredPosition({open, anchorRef, floatingRef, side, align})
  return (
    <>
      <button ref={anchorRef} data-box="anchor">
        anchor
      </button>
      {open && <div ref={floatingRef} data-box="floating" data-testid="floating" style={{top, left}} />}
    </>
  )
}

const placed = () => {
  const style = screen.getByTestId('floating').style
  return {top: parseFloat(style.top), left: parseFloat(style.left)}
}

describe('useAnchoredPosition', () => {
  afterEach(() => vi.restoreAllMocks())

  it('places the element 4px below the anchor, aligned to its start', () => {
    mockBoxes({left: 100, top: 50, width: 100, height: 30}, {width: 150, height: 100})
    render(<Harness open />)
    expect(placed()).toEqual({top: 84, left: 100})
  })

  it('aligns to the end or the centre of the anchor', () => {
    mockBoxes({left: 100, top: 50, width: 100, height: 30}, {width: 150, height: 100})
    const {rerender} = render(<Harness open align="end" />)
    expect(placed().left).toBe(50)
    rerender(<Harness open={false} />)
    rerender(<Harness open align="center" />)
    expect(placed().left).toBe(75)
  })

  it('opens above the anchor for side "top"', () => {
    mockBoxes({left: 100, top: 300, width: 100, height: 30}, {width: 150, height: 100})
    render(<Harness open side="top" />)
    expect(placed().top).toBe(196)
  })

  it('flips above the anchor when there is no room below, and keeps 8px from the viewport edges', () => {
    mockBoxes({left: 1000, top: 700, width: 20, height: 30}, {width: 150, height: 100})
    render(<Harness open />)
    expect(placed()).toEqual({top: 596, left: 1024 - 150 - 8})
  })

  it('follows the anchor on resize and on any scroll while open, and stops listening on close', () => {
    const add = vi.spyOn(window, 'addEventListener')
    const remove = vi.spyOn(window, 'removeEventListener')
    mockBoxes({left: 100, top: 50, width: 100, height: 30}, {width: 150, height: 100})
    const {rerender} = render(<Harness open />)
    expect(add.mock.calls.some(([type]) => type === 'resize')).toBe(true)
    expect(add.mock.calls.some(([type, , options]) => type === 'scroll' && options === true)).toBe(true)

    mockBoxes({left: 120, top: 10, width: 100, height: 30}, {width: 150, height: 100})
    fireEvent.scroll(window)
    expect(placed()).toEqual({top: 44, left: 120})

    rerender(<Harness open={false} />)
    expect(remove.mock.calls.some(([type]) => type === 'resize')).toBe(true)
    expect(remove.mock.calls.some(([type]) => type === 'scroll')).toBe(true)
  })
})
