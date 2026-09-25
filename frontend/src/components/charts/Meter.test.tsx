import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import {Meter} from './Meter'

describe('Meter', () => {
  it('exposes the ratio as a meter and fills proportionally in the given token', () => {
    render(<Meter value={5} max={10} label="Auto-fixable share" token="--success" />)
    const meter = screen.getByRole('meter', {name: 'Auto-fixable share'})
    expect(meter).toHaveAttribute('aria-valuenow', '5')
    expect(meter).toHaveAttribute('aria-valuemax', '10')
    const fill = meter.firstElementChild as HTMLElement
    expect(fill.style.width).toBe('50%')
    expect(fill.style.background).toContain('--success')
  })

  it('clamps out-of-range values and survives a zero maximum', () => {
    const {rerender} = render(<Meter value={15} max={10} label="x" />)
    expect((screen.getByRole('meter').firstElementChild as HTMLElement).style.width).toBe('100%')
    rerender(<Meter value={3} max={0} label="x" />)
    expect((screen.getByRole('meter').firstElementChild as HTMLElement).style.width).toBe('0%')
  })
})
