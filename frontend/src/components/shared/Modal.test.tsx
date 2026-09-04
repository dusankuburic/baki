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
