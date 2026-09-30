import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {createRef} from 'react'
import {MoreHorizontal} from 'lucide-react'
import IconButton from './IconButton'

describe('IconButton', () => {
  it('is named by its label and keeps the icon out of the accessibility tree', () => {
    render(<IconButton icon={MoreHorizontal} label="More actions for contoso-prod" />)
    const button = screen.getByRole('button', {name: 'More actions for contoso-prod'})
    expect(button.querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })

  // Dropdown clones aria-haspopup/aria-expanded/aria-controls onto its
  // trigger; an IconButton trigger used to swallow them.
  it('passes aria-* and data-* attributes through to the <button>', () => {
    render(
      <IconButton
        icon={MoreHorizontal}
        label="More actions"
        aria-haspopup="menu"
        aria-expanded={false}
        aria-controls="menu-1"
        data-testid="kebab"
        id="kebab"
      />,
    )
    const button = screen.getByTestId('kebab')
    expect(button.tagName).toBe('BUTTON')
    expect(button).toHaveAttribute('aria-haspopup', 'menu')
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(button).toHaveAttribute('aria-controls', 'menu-1')
    expect(button).toHaveAttribute('id', 'kebab')
  })

  it('forwards its ref to the <button>, so focus can be returned to it', () => {
    const ref = createRef<HTMLButtonElement>()
    render(<IconButton ref={ref} icon={MoreHorizontal} label="More actions" />)
    expect(ref.current).toBe(screen.getByRole('button', {name: 'More actions'}))
    ref.current?.focus()
    expect(document.activeElement).toBe(ref.current)
  })

  it('does not fire onClick while disabled', () => {
    const onClick = vi.fn()
    render(<IconButton icon={MoreHorizontal} label="More actions" disabled onClick={onClick} />)
    const button = screen.getByRole('button', {name: 'More actions'})
    expect(button).toBeDisabled()
    fireEvent.click(button)
    expect(onClick).not.toHaveBeenCalled()
  })
})
