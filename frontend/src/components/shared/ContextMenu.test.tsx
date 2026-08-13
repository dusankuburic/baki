import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import ContextMenu from './ContextMenu'
import {Trash2, Copy} from 'lucide-react'

describe('ContextMenu a11y', () => {
  it('renders role=menu with role=menuitem children', () => {
    render(
      <ContextMenu x={10} y={10} onClose={() => {}} items={[
        {label: 'Copy', icon: Copy, onClick: () => {}},
        {label: 'Delete', icon: Trash2, onClick: () => {}, variant: 'danger'},
      ]} />,
    )
    expect(screen.getByRole('menu')).toBeInTheDocument()
    expect(screen.getAllByRole('menuitem')).toHaveLength(2)
  })

  it('Esc closes the menu', () => {
    const onClose = vi.fn()
    render(<ContextMenu x={10} y={10} onClose={onClose} items={[{label: 'Copy', icon: Copy, onClick: () => {}}]} />)
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).toHaveBeenCalled()
  })

  it('clicking an item invokes onClick then closes', () => {
    const onClick = vi.fn()
    const onClose = vi.fn()
    render(<ContextMenu x={10} y={10} onClose={onClose} items={[{label: 'Copy', icon: Copy, onClick}]} />)
    fireEvent.click(screen.getByText('Copy'))
    expect(onClick).toHaveBeenCalledOnce()
    expect(onClose).toHaveBeenCalledOnce()
  })
})
