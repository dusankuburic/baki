import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import Modal from './Modal'

// STABLE callbacks: passing a fresh vi.fn() per render changes handleKeyDown's
// identity, which re-runs the surviving modal's effect and re-applies the lock —
// masking the very bug under test.
const noopOuter = () => {}
const noopInner = () => {}

// Modal focuses its first focusable element inside a rAF; the tests below must
// let that frame run or focus never actually leaves the trigger.
async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

beforeEach(() => {
  document.body.style.overflow = ''
})

describe('Modal scroll lock', () => {
  it('locks body scroll while open', () => {
    render(
      <Modal isOpen onClose={noopOuter} title="Outer">
        <p>body</p>
      </Modal>,
    )
    expect(document.body.style.overflow).toBe('hidden')
  })

  // Regression: the cleanup unconditionally reset body overflow, so tearing down
  // an INNER modal unlocked scrolling behind the outer one that was still open.
  // The lock has to be refcounted, not a boolean owned by whichever instance
  // unmounted last.
  it('keeps the lock when a nested modal closes over a still-open parent', () => {
    const {rerender} = render(
      <>
        <Modal isOpen onClose={noopOuter} title="Outer">
          <p>outer</p>
        </Modal>
        <Modal isOpen onClose={noopInner} title="Inner">
          <p>inner</p>
        </Modal>
      </>,
    )
    expect(document.body.style.overflow).toBe('hidden')

    rerender(
      <>
        <Modal isOpen onClose={noopOuter} title="Outer">
          <p>outer</p>
        </Modal>
      </>,
    )

    expect(screen.getByText('outer')).toBeInTheDocument()
    expect(screen.queryByText('inner')).not.toBeInTheDocument()
    expect(document.body.style.overflow).toBe('hidden')
  })

  it('releases the lock once the last modal closes', () => {
    const {rerender} = render(
      <>
        <Modal isOpen onClose={noopOuter} title="Outer">
          <p>outer</p>
        </Modal>
        <Modal isOpen onClose={noopInner} title="Inner">
          <p>inner</p>
        </Modal>
      </>,
    )
    rerender(<></>)
    expect(document.body.style.overflow).toBe('')
  })
})

describe('Modal focus restoration', () => {
  // Regression: focus restore lived only in the isOpen true→false branch, but
  // the prevailing call pattern is `{open && <Modal isOpen … />}` (see
  // App.tsx's SettingsModal), where the modal UNMOUNTS while isOpen is still
  // true — so that branch never ran and focus was dropped on <body>. WCAG 2.4.3.
  it('restores focus to the trigger when the modal unmounts while still open', async () => {
    const trigger = document.createElement('button')
    trigger.textContent = 'Open'
    document.body.appendChild(trigger)
    trigger.focus()

    const {unmount} = render(
      <Modal isOpen onClose={noopOuter} title="Settings">
        <button>Inside</button>
      </Modal>,
    )
    await flushFrame()
    // Precondition: focus genuinely moved into the dialog.
    expect(document.activeElement).not.toBe(trigger)

    unmount()

    expect(document.activeElement).toBe(trigger)
    trigger.remove()
  })

  it('restores focus when the modal is closed via the isOpen prop', async () => {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()

    const {rerender} = render(
      <Modal isOpen onClose={noopOuter} title="Settings">
        <button>Inside</button>
      </Modal>,
    )
    await flushFrame()
    expect(document.activeElement).not.toBe(trigger)

    rerender(
      <Modal isOpen={false} onClose={noopOuter} title="Settings">
        <button>Inside</button>
      </Modal>,
    )

    expect(document.activeElement).toBe(trigger)
    trigger.remove()
  })
})

describe('Modal view transition name', () => {
  it('names the dialog for the View Transitions API when asked', () => {
    render(
      <Modal isOpen onClose={noopOuter} title="Settings" viewTransitionName="settings">
        <p>body</p>
      </Modal>,
    )
    expect(screen.getByRole('dialog').style.viewTransitionName).toBe('settings')
  })

  it('leaves the dialog unnamed by default', () => {
    render(
      <Modal isOpen onClose={noopOuter} title="Plain">
        <p>body</p>
      </Modal>,
    )
    expect(screen.getByRole('dialog').style.viewTransitionName ?? '').toBe('')
  })
})

describe('Modal size and frame variants', () => {
  it('applies the 2xl width, the fill frame and a caller class to the dialog', () => {
    render(
      <Modal isOpen onClose={noopOuter} ariaLabel="Big" size="2xl" height="fill" className="settings-dialog">
        <p>body</p>
      </Modal>,
    )
    const dialog = screen.getByRole('dialog', {name: 'Big'})
    expect(dialog).toHaveClass('max-w-[75rem]')
    expect(dialog).toHaveClass('h-[min(60rem,calc(100vh-3rem))]')
    expect(dialog).toHaveClass('settings-dialog')
  })

  it('goes full screen below 640px when asked, and only then', () => {
    const {rerender} = render(
      <Modal isOpen onClose={noopOuter} title="Add model" fullScreenBelow="sm" footer={<button>Save</button>}>
        <p>body</p>
      </Modal>,
    )
    const dialog = screen.getByRole('dialog', {name: 'Add model'})
    for (const cls of ['max-sm:h-full', 'max-sm:max-w-none', 'max-sm:rounded-none', 'max-sm:flex-col']) {
      expect(dialog).toHaveClass(cls)
    }
    // The body drops its 70vh cap and takes the height between header and footer.
    expect(screen.getByText('body').parentElement).toHaveClass('max-sm:max-h-none', 'max-sm:flex-1')

    rerender(
      <Modal isOpen onClose={noopOuter} title="Add model">
        <p>body</p>
      </Modal>,
    )
    expect(screen.getByRole('dialog', {name: 'Add model'}).className).not.toContain('max-sm:')
  })
})

describe('Modal naming', () => {
  it('is named by its visible title and described by its description', () => {
    render(
      <Modal isOpen onClose={noopOuter} title="Add model" description="Sends one tiny request to test it.">
        <p>body</p>
      </Modal>,
    )
    const dialog = screen.getByRole('dialog', {name: 'Add model'})
    expect(dialog).toHaveAttribute('aria-labelledby', screen.getByRole('heading', {name: 'Add model'}).id)
    expect(dialog).not.toHaveAttribute('aria-label')
    expect(dialog).toHaveAccessibleDescription('Sends one tiny request to test it.')
  })

  it('can be named and described by elements the caller renders', () => {
    render(
      <Modal isOpen onClose={noopOuter} labelledBy="own-heading" describedBy="own-text" ariaLabel="Unused">
        <h2 id="own-heading">Remove resource?</h2>
        <p id="own-text">Its models go with it.</p>
      </Modal>,
    )
    const dialog = screen.getByRole('dialog', {name: 'Remove resource?'})
    expect(dialog).toHaveAccessibleDescription('Its models go with it.')
  })

  it('falls back to ariaLabel without a title, and shows a description above the body', () => {
    render(
      <Modal isOpen onClose={noopOuter} ariaLabel="Settings" description="Everything in one place.">
        <p>body</p>
      </Modal>,
    )
    expect(screen.getByRole('dialog', {name: 'Settings'})).toHaveAccessibleDescription('Everything in one place.')
  })

  // A <button> without a type submits the form around it — here, a form a
  // caller wraps the whole dialog content in.
  it('renders the Close button as a plain button', () => {
    render(
      <Modal isOpen onClose={noopOuter} title="Edit">
        <p>body</p>
      </Modal>,
    )
    expect(screen.getByRole('button', {name: 'Close'})).toHaveAttribute('type', 'button')
  })
})
