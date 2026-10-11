import {describe, it, expect, vi, afterEach} from 'vitest'
import {renderHook, render, screen, fireEvent, act} from '@testing-library/react'
import {getTabbable, useDialogFocus} from './useDialogFocus'
import {useEffect, useRef} from 'react'

describe('useDialogFocus', () => {
  it('Esc calls onClose when open (closeOnEsc default true)', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, containerRef: r})
    })
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('closeOnEsc=false leaves Esc unhandled', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, closeOnEsc: false, containerRef: r})
    })
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).not.toHaveBeenCalled()
  })

  it('listener is removed on unmount', () => {
    const onClose = vi.fn()
    const shared = document.createElement('div')
    const {unmount} = renderHook(() => {
      const r = useRef<HTMLDivElement | null>(null)
      r.current = shared
      useDialogFocus({isOpen: true, onClose, containerRef: r})
    })
    unmount()
    document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    expect(onClose).not.toHaveBeenCalled()
  })
})

describe('useDialogFocus focus restoration', () => {
  async function flushFrame() {
    await act(async () => {
      await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
    })
  }

  function mountOpenDialog() {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()

    const container = document.createElement('div')
    const inner = document.createElement('button')
    container.appendChild(inner)
    document.body.appendChild(container)

    return {trigger, container, inner}
  }

  it('restores focus to the trigger when the dialog unmounts while still open', async () => {
    const {trigger, container, inner} = mountOpenDialog()

    const {unmount} = renderHook(() => {
      const r = useRef<HTMLElement | null>(null)
      r.current = container
      useDialogFocus({isOpen: true, onClose: vi.fn(), containerRef: r})
    })
    await flushFrame()
    expect(document.activeElement).toBe(inner)

    unmount()

    expect(document.activeElement).toBe(trigger)
    trigger.remove()
    container.remove()
  })

  it('keeps focus inside the dialog when onClose identity changes', async () => {
    const {trigger, container, inner} = mountOpenDialog()

    const {rerender} = renderHook(
      ({onClose}: {onClose: () => void}) => {
        const r = useRef<HTMLElement | null>(null)
        r.current = container
        useDialogFocus({isOpen: true, onClose, containerRef: r})
      },
      {initialProps: {onClose: () => {}}},
    )
    await flushFrame()
    expect(document.activeElement).toBe(inner)

    rerender({onClose: () => {}})

    expect(document.activeElement).toBe(inner)
    trigger.remove()
    container.remove()
  })

  function DialogFocusingInside({heading = true}: {heading?: boolean}) {
    const ref = useRef<HTMLDivElement>(null)
    const initial = useRef<HTMLButtonElement>(null)
    useDialogFocus({isOpen: true, onClose: vi.fn(), containerRef: ref, initialFocusRef: initial})
    return (
      <div ref={ref} role="dialog" aria-label="Settings">
        <button ref={initial}>Selected tab</button>
        {heading && <AnchoredHeading />}
      </div>
    )
  }
  function AnchoredHeading() {
    const ref = useRef<HTMLHeadingElement>(null)
    useEffect(() => ref.current?.focus(), [])
    return (
      <h3 ref={ref} tabIndex={-1}>
        Azure AI Foundry
      </h3>
    )
  }

  it('leaves focus where the content put it on open, and still gives it back to the trigger', async () => {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()
    const {unmount} = render(<DialogFocusingInside />)
    await flushFrame()
    expect(screen.getByRole('heading', {name: 'Azure AI Foundry'})).toHaveFocus()
    unmount()
    expect(trigger).toHaveFocus()
    trigger.remove()
  })

  it('still takes its initial focus when nothing inside did', async () => {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()
    const {unmount} = render(<DialogFocusingInside heading={false} />)
    await flushFrame()
    expect(screen.getByRole('button', {name: 'Selected tab'})).toHaveFocus()
    unmount()
    expect(trigger).toHaveFocus()
    trigger.remove()
  })
})

function Dialog({
  label,
  onClose,
  closeOnEsc,
  children,
}: {
  label: string
  onClose: () => void
  closeOnEsc?: boolean
  children?: React.ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  useDialogFocus({isOpen: true, onClose, closeOnEsc, containerRef: ref})
  return (
    <div ref={ref} role="dialog" aria-label={label}>
      {children ?? <button>{`${label} action`}</button>}
    </div>
  )
}

function Nested({
  settingsClose,
  confirmClose,
  confirmOpen,
}: {
  settingsClose: () => void
  confirmClose: () => void
  confirmOpen: boolean
}) {
  return (
    <>
      <Dialog label="Settings" onClose={settingsClose} />
      {confirmOpen && <Dialog label="Confirm" onClose={confirmClose} />}
    </>
  )
}

const escape = (target: Element) => fireEvent.keyDown(target, {key: 'Escape'})
const tab = (target: Element, shiftKey = false) => fireEvent.keyDown(target, {key: 'Tab', shiftKey})

describe('useDialogFocus stack: Escape', () => {
  it('closes only the topmost dialog, and claims the key', () => {
    const settingsClose = vi.fn()
    const confirmClose = vi.fn()
    const {rerender} = render(<Nested settingsClose={settingsClose} confirmClose={confirmClose} confirmOpen={false} />)
    rerender(<Nested settingsClose={settingsClose} confirmClose={confirmClose} confirmOpen />)

    expect(escape(screen.getByRole('button', {name: 'Confirm action'}))).toBe(false)
    expect(confirmClose).toHaveBeenCalledTimes(1)
    expect(settingsClose).not.toHaveBeenCalled()

    rerender(<Nested settingsClose={settingsClose} confirmClose={confirmClose} confirmOpen={false} />)
    escape(screen.getByRole('button', {name: 'Settings action'}))
    expect(settingsClose).toHaveBeenCalledTimes(1)
    expect(confirmClose).toHaveBeenCalledTimes(1)
  })

  it('keeps the confirm on top when the dialog below re-renders with a new onClose', () => {
    const firstSettingsClose = vi.fn()
    const nextSettingsClose = vi.fn()
    const confirmClose = vi.fn()
    const {rerender} = render(
      <Nested settingsClose={firstSettingsClose} confirmClose={confirmClose} confirmOpen={false} />,
    )
    rerender(<Nested settingsClose={firstSettingsClose} confirmClose={confirmClose} confirmOpen />)
    rerender(<Nested settingsClose={nextSettingsClose} confirmClose={confirmClose} confirmOpen />)

    escape(screen.getByRole('button', {name: 'Confirm action'}))
    expect(confirmClose).toHaveBeenCalledTimes(1)
    expect(firstSettingsClose).not.toHaveBeenCalled()
    expect(nextSettingsClose).not.toHaveBeenCalled()

    rerender(<Nested settingsClose={nextSettingsClose} confirmClose={confirmClose} confirmOpen={false} />)
    escape(screen.getByRole('button', {name: 'Settings action'}))
    expect(nextSettingsClose).toHaveBeenCalledTimes(1)
    expect(firstSettingsClose).not.toHaveBeenCalled()
  })

  it('keeps Escape from the dialogs below a top one that handles its own (closeOnEsc: false)', () => {
    const settingsClose = vi.fn()
    const paletteClose = vi.fn()
    render(
      <>
        <Dialog label="Settings" onClose={settingsClose} />
        <Dialog label="Palette" onClose={paletteClose} closeOnEsc={false} />
      </>,
    )
    expect(escape(screen.getByRole('button', {name: 'Palette action'}))).toBe(true)
    expect(paletteClose).not.toHaveBeenCalled()
    expect(settingsClose).not.toHaveBeenCalled()
  })

  it('leaves an Escape that a widget inside the dialog already claimed', () => {
    const onClose = vi.fn()
    render(
      <Dialog label="Settings" onClose={onClose}>
        <input aria-label="Search" onKeyDown={e => e.key === 'Escape' && e.preventDefault()} />
      </Dialog>,
    )
    escape(screen.getByRole('textbox', {name: 'Search'}))
    expect(onClose).not.toHaveBeenCalled()
  })

  it('ignores an Escape that belongs to an IME composition', () => {
    const onClose = vi.fn()
    render(<Dialog label="Settings" onClose={onClose} />)
    fireEvent.keyDown(screen.getByRole('button', {name: 'Settings action'}), {key: 'Escape', isComposing: true})
    expect(onClose).not.toHaveBeenCalled()
  })

  it('keeps the order when a dialog in the middle of the stack closes first', () => {
    const closeA = vi.fn()
    const closeB = vi.fn()
    const closeC = vi.fn()
    function Three({withB}: {withB: boolean}) {
      return (
        <>
          <Dialog label="A" onClose={closeA} />
          {withB && <Dialog label="B" onClose={closeB} />}
          <Dialog label="C" onClose={closeC} />
        </>
      )
    }
    const {rerender} = render(<Three withB />)
    rerender(<Three withB={false} />)

    escape(document.body)
    expect(closeC).toHaveBeenCalledTimes(1)
    expect(closeA).not.toHaveBeenCalled()
    expect(closeB).not.toHaveBeenCalled()
  })
})

describe('useDialogFocus stack: Tab trap', () => {
  it('traps Tab in the topmost dialog only', () => {
    render(
      <>
        <Dialog label="Settings" onClose={vi.fn()}>
          <button>S1</button>
          <button>S2</button>
        </Dialog>
        <Dialog label="Confirm" onClose={vi.fn()}>
          <button>C1</button>
          <button>C2</button>
        </Dialog>
      </>,
    )
    const c1 = screen.getByRole('button', {name: 'C1'})
    const c2 = screen.getByRole('button', {name: 'C2'})
    c2.focus()
    expect(tab(c2)).toBe(false)
    expect(document.activeElement).toBe(c1)
    expect(tab(c1, true)).toBe(false)
    expect(document.activeElement).toBe(c2)

    const s2 = screen.getByRole('button', {name: 'S2'})
    s2.focus()
    expect(tab(s2)).toBe(true)
    expect(document.activeElement).toBe(s2)
  })

  describe('from <body>', () => {
    const nextTask = () => act(() => new Promise<void>(resolve => setTimeout(resolve)))

    async function renderBehindAndDialog() {
      render(
        <>
          <button>Behind</button>
          <Dialog label="Settings" onClose={vi.fn()}>
            <button>First</button>
            <button>Last</button>
          </Dialog>
        </>,
      )
      await act(() => new Promise<void>(resolve => requestAnimationFrame(() => resolve())))
      expect(document.activeElement).toBe(screen.getByRole('button', {name: 'First'}))
      ;(document.activeElement as HTMLElement).blur()
      expect(document.activeElement).toBe(document.body)
    }

    it('lets the browser move first, then brings focus back if it left the dialog', async () => {
      await renderBehindAndDialog()
      expect(tab(document.body)).toBe(true)
      screen.getByRole('button', {name: 'Behind'}).focus()
      await nextTask()
      expect(document.activeElement).toBe(screen.getByRole('button', {name: 'First'}))

      ;(document.activeElement as HTMLElement).blur()
      tab(document.body, true)
      screen.getByRole('button', {name: 'Behind'}).focus()
      await nextTask()
      expect(document.activeElement).toBe(screen.getByRole('button', {name: 'Last'}))
    })

    it('keeps the stop the browser chose inside the dialog', async () => {
      await renderBehindAndDialog()
      tab(document.body)
      screen.getByRole('button', {name: 'Last'}).focus()
      await nextTask()
      expect(document.activeElement).toBe(screen.getByRole('button', {name: 'Last'}))
    })
  })

  it('wraps from the <summary> of a collapsed <details> at the end', () => {
    render(
      <Dialog label="Settings" onClose={vi.fn()}>
        <button>First</button>
        <details>
          <summary>Use a personal access token</summary>
          <input aria-label="Token" />
        </details>
      </Dialog>,
    )
    const first = screen.getByRole('button', {name: 'First'})
    const summary = screen.getByText('Use a personal access token')
    summary.focus()
    expect(tab(summary)).toBe(false)
    expect(document.activeElement).toBe(first)
    expect(tab(first, true)).toBe(false)
    expect(document.activeElement).toBe(summary)
  })

  it('counts the contents of an open <details>', () => {
    render(
      <Dialog label="Settings" onClose={vi.fn()}>
        <button>First</button>
        <details open>
          <summary>Advanced</summary>
          <input aria-label="Token" />
        </details>
      </Dialog>,
    )
    const token = screen.getByRole('textbox', {name: 'Token'})
    token.focus()
    expect(tab(token)).toBe(false)
    expect(document.activeElement).toBe(screen.getByRole('button', {name: 'First'}))
  })

  it('wraps from an element Tab does not stop at when nothing follows it', () => {
    render(
      <Dialog label="Settings" onClose={vi.fn()}>
        <h4 tabIndex={-1}>Top</h4>
        <button>First</button>
        <h4 tabIndex={-1}>Middle</h4>
        <button>Last</button>
        <h4 tabIndex={-1}>Bottom</h4>
      </Dialog>,
    )
    const bottom = screen.getByText('Bottom')
    bottom.focus()
    expect(tab(bottom)).toBe(false)
    expect(document.activeElement).toBe(screen.getByRole('button', {name: 'First'}))

    const top = screen.getByText('Top')
    top.focus()
    expect(tab(top, true)).toBe(false)
    expect(document.activeElement).toBe(screen.getByRole('button', {name: 'Last'}))

    const middle = screen.getByText('Middle')
    middle.focus()
    expect(tab(middle)).toBe(true)
    expect(tab(middle, true)).toBe(true)
  })

  it('leaves a Tab that a control inside the dialog claimed (an editor indenting)', () => {
    render(
      <Dialog label="Settings" onClose={vi.fn()}>
        <button>First</button>
        <textarea aria-label="Code" onKeyDown={e => e.key === 'Tab' && e.preventDefault()} />
      </Dialog>,
    )
    const code = screen.getByRole('textbox', {name: 'Code'})
    code.focus()
    tab(code)
    expect(document.activeElement).toBe(code)
  })

  it('focuses the first element Tab can reach on open', async () => {
    render(
      <Dialog label="Settings" onClose={vi.fn()}>
        <button disabled>Off</button>
        <button tabIndex={-1}>Skipped</button>
        <button>On</button>
      </Dialog>,
    )
    await act(async () => {
      await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
    })
    expect(document.activeElement).toBe(screen.getByRole('button', {name: 'On'}))
  })
})

describe('getTabbable', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    document.querySelectorAll('[data-tabbable-fixture]').forEach(el => el.remove())
  })

  function mount(html: string) {
    const container = document.createElement('div')
    container.setAttribute('data-tabbable-fixture', '')
    container.innerHTML = html
    document.body.appendChild(container)
    return container
  }

  const names = (els: HTMLElement[]) => els.map(el => el.getAttribute('aria-label') ?? el.textContent?.trim())

  it('keeps only what Tab stops at, in document order', () => {
    const container = mount(`
      <button>first</button>
      <button tabindex="-1">negative tabindex</button>
      <button disabled>disabled</button>
      <fieldset disabled><button>in a disabled fieldset</button></fieldset>
      <div hidden><button>in [hidden]</button></div>
      <div inert><button>in [inert]</button></div>
      <div style="visibility: hidden"><button>invisible</button></div>
      <input type="hidden" aria-label="hidden input" />
      <a>anchor without href</a>
      <a href="#x">link</a>
      <span tabindex="0">span stop</span>
      <div tabindex="nope">unparsable tabindex</div>
      <details><summary>closed toggle</summary><input aria-label="inside closed" /></details>
      <details open><summary>open toggle</summary><input aria-label="inside open" /></details>
      <summary>stray summary</summary>
      <button>last</button>
    `)
    expect(names(getTabbable(container))).toEqual([
      'first',
      'link',
      'span stop',
      'closed toggle',
      'open toggle',
      'inside open',
      'last',
    ])
  })

  it('follows nested <details>: only the toggles of the open levels are reachable', () => {
    const container = mount(`
      <details open>
        <summary>outer</summary>
        <details><summary>inner</summary><button>deep</button></details>
      </details>
      <details>
        <summary>closed outer</summary>
        <details open><summary>open inside closed</summary><button>unreachable</button></details>
      </details>
    `)
    expect(names(getTabbable(container))).toEqual(['outer', 'inner', 'closed outer'])
  })

  it('drops elements without a box once the page has layout', () => {
    const container = mount(
      `<button>shown</button><button data-no-box>display none</button><button>also shown</button>`,
    )
    vi.spyOn(Element.prototype, 'getClientRects').mockImplementation(function (this: Element) {
      return (this.hasAttribute('data-no-box') ? [] : [{}]) as unknown as DOMRectList
    })
    expect(names(getTabbable(container))).toEqual(['shown', 'also shown'])
  })

  it('keeps everything when nothing has a box (no layout)', () => {
    const container = mount(`<button>a</button><button>b</button>`)
    expect(getTabbable(container)).toHaveLength(2)
  })
})
