import {describe, it, expect, vi, afterEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {MoreHorizontal} from 'lucide-react'
import Dropdown, {type DropdownItem} from './Dropdown'
import IconButton from './IconButton'

function open() {
  fireEvent.click(screen.getByText('Open'))
  return screen.getByRole('menu')
}

function arrowDown(menu: HTMLElement, times: number) {
  for (let i = 0; i < times; i++) fireEvent.keyDown(menu, {key: 'ArrowDown'})
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('Dropdown keyboard selection with groups', () => {
  it('Enter invokes the focused item, not its neighbour', () => {
    const onA = vi.fn()
    const onB = vi.fn()
    const onC = vi.fn()
    const onD = vi.fn()
    const items: DropdownItem[] = [
      {
        type: 'group',
        label: 'Group',
        items: [
          {type: 'item', label: 'A', onSelect: onA},
          {type: 'item', label: 'B', onSelect: onB},
        ],
      },
      {type: 'item', label: 'C', onSelect: onC},
      {type: 'item', label: 'D', onSelect: onD},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)

    const menu = open()
    arrowDown(menu, 3)
    fireEvent.keyDown(menu, {key: 'Enter'})

    expect(onC).toHaveBeenCalledTimes(1)
    expect(onD).not.toHaveBeenCalled()
    expect(onA).not.toHaveBeenCalled()
    expect(onB).not.toHaveBeenCalled()
  })

  it('Enter activates the last item after a leading group', () => {
    const onLast = vi.fn()
    const items: DropdownItem[] = [
      {type: 'item', label: 'A', onSelect: vi.fn()},
      {
        type: 'group',
        label: 'Group',
        items: [
          {type: 'item', label: 'B', onSelect: vi.fn()},
          {type: 'item', label: 'C', onSelect: vi.fn()},
        ],
      },
      {type: 'item', label: 'Last', onSelect: onLast},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)

    const menu = open()
    arrowDown(menu, 4)
    fireEvent.keyDown(menu, {key: 'Enter'})

    expect(onLast).toHaveBeenCalledTimes(1)
  })

  it('still selects correctly in a flat menu with separators', () => {
    const onSecond = vi.fn()
    const items: DropdownItem[] = [
      {type: 'item', label: 'First', onSelect: vi.fn()},
      {type: 'separator'},
      {type: 'item', label: 'Second', onSelect: onSecond},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)

    const menu = open()
    arrowDown(menu, 2)
    fireEvent.keyDown(menu, {key: 'Enter'})

    expect(onSecond).toHaveBeenCalledTimes(1)
  })

  it('skips disabled items when roving', () => {
    const onEnabled = vi.fn()
    const onDisabled = vi.fn()
    const items: DropdownItem[] = [
      {type: 'item', label: 'Disabled', onSelect: onDisabled, disabled: true},
      {type: 'item', label: 'Enabled', onSelect: onEnabled},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)

    const menu = open()
    arrowDown(menu, 1)
    fireEvent.keyDown(menu, {key: 'Enter'})

    expect(onEnabled).toHaveBeenCalledTimes(1)
    expect(onDisabled).not.toHaveBeenCalled()
  })
})

describe('Dropdown repositioning', () => {
  it('tracks scroll and resize while open, and unsubscribes on close', () => {
    const addSpy = vi.spyOn(window, 'addEventListener')
    const removeSpy = vi.spyOn(window, 'removeEventListener')
    const items: DropdownItem[] = [{type: 'item', label: 'A', onSelect: vi.fn()}]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)

    const menu = open()
    expect(addSpy.mock.calls.some(([type]) => type === 'scroll')).toBe(true)
    expect(addSpy.mock.calls.some(([type]) => type === 'resize')).toBe(true)

    fireEvent.keyDown(menu, {key: 'Escape'})
    expect(removeSpy.mock.calls.some(([type]) => type === 'scroll')).toBe(true)
    expect(removeSpy.mock.calls.some(([type]) => type === 'resize')).toBe(true)
  })
})

describe('Dropdown roving focus', () => {
  function renderTwoItems() {
    const items: DropdownItem[] = [
      {type: 'item', label: 'A', onSelect: vi.fn()},
      {type: 'item', label: 'B', onSelect: vi.fn()},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)
    return open()
  }

  it('emits data-index and menu-item ids on every row', () => {
    const menu = renderTwoItems()
    const btns = Array.from(menu.querySelectorAll<HTMLElement>('button[role="menuitem"]'))
    expect(btns.map(b => b.dataset.index)).toEqual(['0', '1'])
    expect(btns.map(b => b.id)).toEqual(['menu-item-0', 'menu-item-1'])
  })

  it('numbers rows continuously across a group boundary', () => {
    const items: DropdownItem[] = [
      {type: 'item', label: 'A', onSelect: vi.fn()},
      {type: 'group', label: 'G', items: [{type: 'item', label: 'B', onSelect: vi.fn()}]},
      {type: 'separator'},
      {type: 'item', label: 'C', onSelect: vi.fn()},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)
    const menu = open()
    const btns = Array.from(menu.querySelectorAll<HTMLElement>('button[role="menuitem"]'))
    expect(btns.map(b => b.dataset.index)).toEqual(['0', '1', '2'])
  })

  it('moves real DOM focus onto the active row and tracks aria-activedescendant', () => {
    const menu = renderTwoItems()
    arrowDown(menu, 1)
    expect(menu.getAttribute('aria-activedescendant')).toBe('menu-item-0')
    expect((document.activeElement as HTMLElement)?.dataset.index).toBe('0')

    arrowDown(menu, 1)
    expect(menu.getAttribute('aria-activedescendant')).toBe('menu-item-1')
    expect((document.activeElement as HTMLElement)?.dataset.index).toBe('1')
  })
})

describe('Dropdown Escape', () => {
  it('claims the Escape that closes the menu so parent shortcuts ignore it', () => {
    const items: DropdownItem[] = [{type: 'item', label: 'A', onSelect: vi.fn()}]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)
    const menu = open()
    expect(fireEvent.keyDown(menu, {key: 'Escape'})).toBe(false)
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })
})

describe('Dropdown trigger semantics', () => {
  const items: DropdownItem[] = [{type: 'item', label: 'A', onSelect: vi.fn()}]

  it('puts aria-haspopup, aria-expanded and aria-controls on the trigger button', () => {
    render(<Dropdown trigger={<button>Open</button>} items={items} />)
    const trigger = screen.getByRole('button', {name: 'Open'})
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(trigger).not.toHaveAttribute('aria-controls')
    expect(trigger.parentElement).not.toHaveAttribute('aria-haspopup')
    expect(trigger.parentElement).not.toHaveAttribute('aria-expanded')

    fireEvent.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(trigger).toHaveAttribute('aria-controls', screen.getByRole('menu').id)
  })

  it('reaches the <button> of an IconButton trigger', () => {
    render(<Dropdown trigger={<IconButton icon={MoreHorizontal} label="More actions" />} items={items} />)
    const trigger = screen.getByRole('button', {name: 'More actions'})
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    fireEvent.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
  })

  it('renders its items as plain buttons, which do not submit a surrounding form', () => {
    const onSelect = vi.fn()
    const submit = vi.fn((e: React.FormEvent) => e.preventDefault())
    render(
      <form onSubmit={submit}>
        <Dropdown trigger={<button type="button">Open</button>} items={[{type: 'item', label: 'Rename', onSelect}]} />
      </form>,
    )
    open()
    const item = screen.getByRole('menuitem', {name: 'Rename'})
    expect(item).toHaveAttribute('type', 'button')
    fireEvent.click(item)
    expect(onSelect).toHaveBeenCalledTimes(1)
    expect(submit).not.toHaveBeenCalled()
  })
})

describe('Dropdown disabledReason', () => {
  function renderMenu() {
    const onEdit = vi.fn()
    const onTest = vi.fn()
    const onRemove = vi.fn()
    const items: DropdownItem[] = [
      {type: 'item', label: 'Edit', onSelect: onEdit},
      {type: 'item', label: 'Test', onSelect: onTest, disabledReason: 'Enter the credentials first'},
      {type: 'separator'},
      {type: 'item', label: 'Remove', onSelect: onRemove, danger: true},
    ]
    render(<Dropdown trigger={<button>Open</button>} items={items} />)
    return {menu: open(), onEdit, onTest, onRemove}
  }

  it('keeps the item reachable, marked unavailable, with the reason as its description', () => {
    const {menu} = renderMenu()
    const test = screen.getByRole('menuitem', {name: 'Test'})
    expect(test).toHaveAttribute('aria-disabled', 'true')
    expect(test).toHaveAccessibleDescription('Enter the credentials first')
    expect(screen.getByText('Enter the credentials first')).toBeVisible()

    arrowDown(menu, 2)
    expect(document.activeElement).toBe(test)
  })

  it('never runs the unavailable action, by key or by click, and stays open', () => {
    const {menu, onTest, onRemove} = renderMenu()
    arrowDown(menu, 2)
    fireEvent.keyDown(menu, {key: 'Enter'})
    fireEvent.click(screen.getByRole('menuitem', {name: 'Test'}))
    expect(onTest).not.toHaveBeenCalled()
    expect(screen.getByRole('menu')).toBeInTheDocument()

    arrowDown(menu, 1)
    fireEvent.keyDown(menu, {key: 'Enter'})
    expect(onRemove).toHaveBeenCalledTimes(1)
  })
})
