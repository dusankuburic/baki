import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {Eye, MoreHorizontal} from 'lucide-react'
import Button from './Button'
import IconButton from './IconButton'
import Checkbox from './Checkbox'

describe('Button', () => {
  // Preflight makes every <svg> display:block, so an icon passed as a CHILD
  // lands on its own line above the label (the Compose dialog's Preview and
  // Send buttons showed the icon stacked over the text). The `icon` prop
  // renders it as a flex sibling of the label span instead.
  it('renders the icon as a sibling of the label, not inside it', () => {
    render(<Button icon={Eye}>Preview</Button>)
    const button = screen.getByRole('button', {name: 'Preview'})
    const [first, second] = Array.from(button.children)
    expect(first.tagName.toLowerCase()).toBe('svg')
    expect(second.tagName.toLowerCase()).toBe('span')
    expect(second).toHaveTextContent('Preview')
  })

  it('greys out a disabled primary button instead of fading the brand fill', () => {
    render(
      <Button variant="primary" disabled>
        Send
      </Button>,
    )
    const button = screen.getByRole('button', {name: 'Send'})
    expect(button).toBeDisabled()
    expect(button.className).not.toContain('opacity-50')
    expect(button.className).not.toContain('bg-brand-500')
    expect(button.className).toContain('bg-surface-3')
  })

  it('keeps the translucent disabled look for the other variants', () => {
    render(
      <Button variant="secondary" disabled>
        Cancel
      </Button>,
    )
    expect(screen.getByRole('button', {name: 'Cancel'}).className).toContain('opacity-50')
  })
})

// A <button> without a type is a SUBMIT button. The shared buttons default to
// type="button", so dropping one into a form no longer submits it; a submit
// button says so (every form in the app already does).
describe('button types', () => {
  function renderInForm(children: React.ReactNode) {
    const submit = vi.fn((e: React.FormEvent) => e.preventDefault())
    render(<form onSubmit={submit}>{children}</form>)
    return submit
  }

  it('Button defaults to type="button" and keeps an explicit submit', () => {
    const submit = renderInForm(
      <>
        <Button>Preview</Button>
        <Button type="submit">Save</Button>
      </>,
    )
    const preview = screen.getByRole('button', {name: 'Preview'})
    expect(preview).toHaveAttribute('type', 'button')
    fireEvent.click(preview)
    expect(submit).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    expect(submit).toHaveBeenCalledTimes(1)
  })

  it('IconButton defaults to type="button"', () => {
    const submit = renderInForm(<IconButton icon={MoreHorizontal} label="More actions" />)
    const more = screen.getByRole('button', {name: 'More actions'})
    expect(more).toHaveAttribute('type', 'button')
    fireEvent.click(more)
    expect(submit).not.toHaveBeenCalled()
  })

  it('Checkbox is a plain button', () => {
    const onChange = vi.fn()
    const submit = renderInForm(<Checkbox checked={false} onChange={onChange} label="Use for knowledge base" />)
    const box = screen.getByRole('checkbox')
    expect(box).toHaveAttribute('type', 'button')
    fireEvent.click(box)
    expect(onChange).toHaveBeenCalledWith(true)
    expect(submit).not.toHaveBeenCalled()
  })
})
