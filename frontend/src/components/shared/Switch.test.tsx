import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Switch from './Switch'

describe('Switch', () => {
  it('keeps the accessible name when the label is visually hidden', () => {
    const onChange = vi.fn()
    render(<Switch label="Save history" labelHidden checked={false} onChange={onChange} />)
    const control = screen.getByRole('switch', {name: 'Save history'})
    expect(screen.getByText('Save history')).toHaveClass('sr-only')
    // Never a submit button: rows live inside forms elsewhere.
    expect(control).toHaveAttribute('type', 'button')
    fireEvent.click(control)
    expect(onChange).toHaveBeenCalledWith(true)
  })

  it('shows the label by default', () => {
    render(<Switch label="Visible" checked onChange={() => {}} />)
    expect(screen.getByText('Visible')).not.toHaveClass('sr-only')
  })
})
