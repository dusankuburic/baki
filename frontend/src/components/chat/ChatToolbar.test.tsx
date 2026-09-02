import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import ChatToolbar from './ChatToolbar'

describe('ChatToolbar tools toggle', () => {
  it('renders the toggle with the standard tooltip for capable providers', () => {
    render(<ChatToolbar messageCount={0} onNewChat={vi.fn()} onClearContext={vi.fn()} onCompact={vi.fn()} useTools onToggleTools={vi.fn()} providerId="claude" />)
    const toggle = screen.getByRole('button', {name: /tools on/i})
    expect(toggle).toHaveAttribute('aria-pressed', 'true')
    expect(toggle).toHaveAttribute('title', expect.stringContaining('Claude'))
  })

  it('notes the marker protocol for Copilot', () => {
    render(<ChatToolbar messageCount={0} onNewChat={vi.fn()} onClearContext={vi.fn()} onCompact={vi.fn()} onToggleTools={vi.fn()} providerId="copilot" />)
    expect(screen.getByRole('button', {name: /^tools$/i})).toHaveAttribute('title', expect.stringContaining('text-marker protocol'))
  })

  it('hides the toggle when onToggleTools is undefined (demo provider)', () => {
    render(<ChatToolbar messageCount={0} onNewChat={vi.fn()} onClearContext={vi.fn()} onCompact={vi.fn()} />)
    expect(screen.queryByRole('button', {name: /tools/i})).not.toBeInTheDocument()
  })
})
