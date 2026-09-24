import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import {Eye} from 'lucide-react'
import Button from './Button'

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
