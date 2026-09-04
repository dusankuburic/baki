import {describe, it, expect, vi, afterEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Dropdown, {type DropdownItem} from './Dropdown'

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

// Regression: flattenItems assigned `_index` from a counter that advanced for a
// `group` WRAPPER as well as for each of its children, so `_index` diverged from
// the flattened array position as soon as a group was present. activeIndex holds
// `_index` values, but the Enter branch used it as an ARRAY index — so Enter
// either fired a DIFFERENT item's onSelect or silently did nothing.
//
//   items = [{group: [A, B]}, C, D]
//     result   = [A, B, C, D]       (array positions 0,1,2,3)
//     _index   =  0  1  3  4        ← C is at position 2 but carries _index 3
//   focusing C sets activeIndex=3 → flattenedItems[3] is D.
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
    arrowDown(menu, 3) // A → B → C
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
    arrowDown(menu, 4) // A → B → C → Last
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

// Regression: the menu is position:fixed and was positioned ONCE on open, so any
// scroll or resize left it floating away from its trigger. SourceFilePicker
// already subscribed to both events; Dropdown did not.
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

// Regression: renderItems was handed the ORIGINAL nested items array, whose
// entries carry no `_index`, so `data-index` and the `menu-item-N` ids were
// never emitted. The roving-focus effect looks items up by `dataset.index`, so
// it matched nothing — arrow keys moved no real DOM focus and highlighted no
// row, in EVERY dropdown. Screen readers had nothing to announce.
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
