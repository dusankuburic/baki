import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {useRef} from 'react'
import RecentFilesMenu from './RecentFilesMenu'
import type {RecentFile} from '@/types'

const files: RecentFile[] = [
  {path: '/f1', name: 'Folder One', size: 0, lastOpen: '2024-01-01T00:00:00Z', isFolder: true},
  {path: '/a.txt', name: 'a.txt', size: 100, lastOpen: '2024-01-02T00:00:00Z'},
  {path: '/b.txt', name: 'b.txt', size: 200, lastOpen: '2024-01-03T00:00:00Z'},
]

// A tiny host so the menu has a real anchorRef (a DOM element) to position
// against, matching how FileHeader wires it up.
function Host(props: Partial<React.ComponentProps<typeof RecentFilesMenu>>) {
  const anchorRef = useRef<HTMLDivElement>(null)
  return (
    <div>
      <div ref={anchorRef} data-testid="anchor" />
      <RecentFilesMenu
        files={files}
        anchorRef={anchorRef}
        onSelect={vi.fn()}
        onRemove={vi.fn()}
        onClear={vi.fn()}
        onClose={vi.fn()}
        {...props}
      />
    </div>
  )
}

describe('RecentFilesMenu', () => {
  it('renders folders and files in separate groups', () => {
    render(<Host />)
    expect(screen.getByText('Folders')).toBeInTheDocument()
    expect(screen.getByText('Files')).toBeInTheDocument()
    expect(screen.getByText('Folder One')).toBeInTheDocument()
    expect(screen.getByText('a.txt')).toBeInTheDocument()
  })

  it('renders into document.body via Portal (escapes an overflow:hidden ancestor)', () => {
    const {container} = render(
      <div style={{overflow: 'hidden', height: 10}}>
        <Host />
      </div>,
    )
    // The menu's listbox should NOT be a descendant of the clipping container.
    const clippingContainer = container.firstChild as HTMLElement
    const menu = screen.getByRole('listbox')
    expect(clippingContainer.contains(menu)).toBe(false)
    expect(document.body.contains(menu)).toBe(true)
  })

  it('navigates with arrow keys and opens the focused item on Enter', () => {
    const onSelect = vi.fn()
    render(<Host onSelect={onSelect} />)
    const menu = screen.getByRole('listbox')
    // Order is folders-then-files: Folder One(0), a.txt(1), b.txt(2).
    fireEvent.keyDown(menu, {key: 'ArrowDown'})
    fireEvent.keyDown(menu, {key: 'ArrowDown'})
    fireEvent.keyDown(menu, {key: 'Enter'})
    expect(onSelect).toHaveBeenCalledWith('/a.txt')
  })

  it('closes on Escape', () => {
    const onClose = vi.fn()
    render(<Host onClose={onClose} />)
    fireEvent.keyDown(screen.getByRole('listbox'), {key: 'Escape'})
    expect(onClose).toHaveBeenCalled()
  })

  it('closes when clicking outside the menu', () => {
    const onClose = vi.fn()
    render(
      <div>
        <button>outside</button>
        <Host onClose={onClose} />
      </div>,
    )
    fireEvent.mouseDown(screen.getByText('outside'))
    expect(onClose).toHaveBeenCalled()
  })

  it('shows an empty state and hides Clear all when there are no recents', () => {
    render(<Host files={[]} />)
    expect(screen.getByText('No recent items')).toBeInTheDocument()
    expect(screen.queryByText('Clear all')).not.toBeInTheDocument()
  })
})
