import {useEffect, useLayoutEffect, useRef} from 'react'
import {isImeKeydown} from '@/lib/imeKey'

// Elements the browser can focus without a tabindex. Of the summaries, only
// the toggle of a <details> is focusable; getTabbable() checks that.
const NATIVELY_FOCUSABLE = [
  'a[href]',
  'area[href]',
  'button',
  'input:not([type="hidden"])',
  'select',
  'textarea',
  'summary',
  'iframe',
  '[contenteditable]:not([contenteditable="false"])',
].join(', ')

// Candidates for the Tab trap. The selector over-counts on purpose (a
// tabindex="-1" element, a disabled button, the contents of a collapsed
// <details>); getTabbable() drops what Tab cannot actually reach.
export const FOCUSABLE = `${NATIVELY_FOCUSABLE}, [tabindex]`

/**
 * The elements inside `container` that Tab stops at, in document order.
 * Left out: a negative tabindex, disabled controls, anything inert or
 * [hidden] (or inside one), the contents of a closed <details> (its summary
 * stays), and elements that are not rendered or are visibility:hidden.
 *
 * A trap whose first or last element is one the browser skips never wraps:
 * focus walks past it and out of the dialog. That is how Tab used to leave
 * Settings through the collapsed "use a personal access token" <details> at
 * the end of AI Accounts — its hidden input was the trap's "last" element,
 * while the real last stop, the <summary>, was not in the list at all.
 */
export function getTabbable(container: HTMLElement): HTMLElement[] {
  // Without a layout engine (jsdom) no element has client rects, so the
  // rendered check applies only when the container itself has a box.
  const hasLayout = container.getClientRects().length > 0
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(el => isTabbable(el, hasLayout))
}

function isTabbable(el: HTMLElement, hasLayout: boolean): boolean {
  const tabindex = el.getAttribute('tabindex')
  if (tabindex !== null) {
    const n = Number.parseInt(tabindex, 10)
    // An unparsable tabindex is ignored, so only a natively focusable
    // element keeps its stop.
    if (Number.isNaN(n) ? !el.matches(NATIVELY_FOCUSABLE) : n < 0) return false
  }
  if (el.matches(':disabled')) return false
  if (el.localName === 'summary' && !isDetailsToggle(el)) return false
  if (el.closest('[inert], [hidden]')) return false
  if (inClosedDetails(el)) return false
  if (hasLayout && el.getClientRects().length === 0) return false
  const {visibility} = getComputedStyle(el)
  return visibility !== 'hidden' && visibility !== 'collapse'
}

// The first <summary> child of a <details> is its toggle.
function isDetailsToggle(el: Element): boolean {
  const parent = el.parentElement
  return parent?.localName === 'details' && parent.querySelector(':scope > summary') === el
}

// A closed <details> renders its toggle and nothing else, so walking up from
// the element, every step into a closed <details> has to come from its
// toggle. (The contents are not display:none in every engine — Chromium
// skips them with content-visibility — so the client-rect check alone
// cannot be trusted with them.)
function inClosedDetails(el: HTMLElement): boolean {
  let node: Element = el
  let parent = node.parentElement
  while (parent) {
    if (parent.localName === 'details' && !(parent as HTMLDetailsElement).open && !isDetailsToggle(node)) return true
    node = parent
    parent = node.parentElement
  }
  return false
}

// ---- The dialog stack ------------------------------------------------------
//
// Dialogs nest: Settings opens a confirm, the chat's focus mode opens the
// Compose editor. Only the TOPMOST open dialog may act on Escape and Tab.
// Each dialog used to listen on its own, so one Escape in a confirm opened
// from Settings closed the confirm AND Settings, and Settings' trap ran for
// keys that belonged to the confirm.
//
// One document listener serves the whole stack and dispatches to its top
// entry. An entry is pushed when its dialog opens and removed when it closes
// — never on a re-render: the entry reads the dialog's latest props through a
// ref, so a new onClose identity (a parent re-render while a confirm is open)
// cannot move Settings back above the confirm, which is what re-subscribing
// the listener on every onClose change used to do.

type DialogProps = {
  onClose: () => void
  closeOnEsc: boolean
  containerRef: React.RefObject<HTMLElement | null>
}

type DialogEntry = {latest: {current: DialogProps}}

const dialogStack: DialogEntry[] = []

function pushDialog(entry: DialogEntry) {
  dialogStack.push(entry)
  if (dialogStack.length === 1) document.addEventListener('keydown', handleDialogKeyDown)
}

function removeDialog(entry: DialogEntry) {
  // Wherever it is: a lower dialog can close while one above it stays open.
  const index = dialogStack.indexOf(entry)
  if (index !== -1) dialogStack.splice(index, 1)
  if (dialogStack.length === 0) document.removeEventListener('keydown', handleDialogKeyDown)
}

function handleDialogKeyDown(e: KeyboardEvent) {
  const top = dialogStack[dialogStack.length - 1]
  if (!top) return
  const {onClose, closeOnEsc, containerRef} = top.latest.current
  if (e.key === 'Escape') {
    // defaultPrevented: a nested widget inside the dialog (an autocomplete
    // menu, a combobox, a search box clearing its query) handles Escape
    // first to dismiss ITSELF. Without this check the same keypress also
    // tore down the whole dialog, so one Escape closed both the menu and the
    // window it was opened from. A dialog with closeOnEsc:false handles its
    // own Escape; the ones below it still must not.
    if (!closeOnEsc || e.defaultPrevented || isImeKeydown(e)) return
    // Claimed, so the listeners after this one (the chat's Escape) leave it.
    e.preventDefault()
    onClose()
  } else if (e.key === 'Tab' && !e.defaultPrevented && !e.ctrlKey && !e.altKey && !e.metaKey) {
    const container = containerRef.current
    if (container) trapTab(e, container)
  }
}

function trapTab(e: KeyboardEvent, container: HTMLElement) {
  const current = document.activeElement
  if (!(current instanceof HTMLElement) || !container.contains(current)) {
    // Focus is on <body>: a click on text, or on a backdrop that does not
    // close the dialog. The browser tabs on from where the click landed,
    // which is right while a stop follows that point inside the dialog; with
    // none it walks out to the page behind. Let it move, then bring focus
    // back if it left. Focus anywhere else outside the dialog (a toast) is
    // the browser's business.
    if (!current || current === document.body) returnFocusIfItLeaves(container, e.shiftKey)
    return
  }
  const tabbable = getTabbable(container)
  if (tabbable.length === 0) {
    e.preventDefault()
    return
  }
  const first = tabbable[0]
  const last = tabbable[tabbable.length - 1]
  // Non-empty (early return above); the guard only satisfies the type.
  if (!first || !last) return
  const wrapTo = e.shiftKey ? last : first
  if (tabbable.includes(current)) {
    if (current === (e.shiftKey ? first : last)) {
      e.preventDefault()
      wrapTo.focus()
    }
    return
  }
  // Focus sits on something Tab does not stop at (a tabindex="-1" heading
  // focused on purpose, a roving option): the browser goes to the next stop
  // in document order, so wrap only when the dialog has none on that side.
  const hasNext = tabbable.some(el => (e.shiftKey ? follows(current, el) : follows(el, current)))
  if (!hasNext) {
    e.preventDefault()
    wrapTo.focus()
  }
}

// After the browser's own Tab from <body> (the next task): if focus landed
// outside a dialog that is still on top, move it to the dialog's first stop,
// or its last one for Shift+Tab.
function returnFocusIfItLeaves(container: HTMLElement, backwards: boolean) {
  setTimeout(() => {
    const now = document.activeElement
    if (!now || now === document.body || container.contains(now)) return
    if (dialogStack[dialogStack.length - 1]?.latest.current.containerRef.current !== container) return
    const tabbable = getTabbable(container)
    tabbable[backwards ? tabbable.length - 1 : 0]?.focus()
  })
}

// follows(a, b): a comes after b in document order (a descendant counts as
// after its ancestor).
function follows(a: Node, b: Node): boolean {
  return (b.compareDocumentPosition(a) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
}

// useDialogFocus applies the three behaviours an overlay dialog needs for
// keyboard/screen-reader parity: Tab focus trap (cycles within the dialog),
// Esc-to-close, and focus restoration to the trigger on close. Mobile drawers,
// CommandPalette, GlobalSearch and the shared <Modal> all get all three from
// here (WCAG 2.4.3 / 2.1.2) — <Modal> used to carry its own byte-identical copy,
// which meant fixing a trap bug required finding both.
//
// containerRef points at the dialog's root element. isOpen toggles activation.
// On open, the first tabbable element (or initialFocusRef) is auto-focused a
// frame later — unless the dialog's content has put focus inside it by then;
// on close, the previously-focused element (the trigger) is restored. Nested
// dialogs stack: only the one opened last handles Escape and Tab (see the
// stack above).
export function useDialogFocus(opts: {
  isOpen: boolean
  onClose: () => void
  closeOnEsc?: boolean
  containerRef: React.RefObject<HTMLElement | null>
  // Element to focus on open instead of the first focusable one — e.g. an
  // editor dialog whose first focusable element is its Close button.
  initialFocusRef?: React.RefObject<HTMLElement | null>
}) {
  const {isOpen, onClose, closeOnEsc = true, containerRef, initialFocusRef} = opts
  const previousFocusRef = useRef<HTMLElement | null>(null)

  // The props the stack's keydown handler reads. Refreshed after every
  // commit, so a handler never runs a stale onClose.
  const latestRef = useRef<DialogProps>({onClose, closeOnEsc, containerRef})
  useLayoutEffect(() => {
    latestRef.current = {onClose, closeOnEsc, containerRef}
  })

  // Stack membership, keyed on isOpen ALONE (see the stack above). A layout
  // effect: the entry is in place as soon as the dialog is on screen, before
  // a keypress can reach the dialog below it.
  useLayoutEffect(() => {
    if (!isOpen) return
    const entry: DialogEntry = {latest: latestRef}
    pushDialog(entry)
    return () => removeDialog(entry)
  }, [isOpen])

  // The element to restore focus to on close: the one focused when the
  // dialog opened. Captured in the layout phase, before any passive effect
  // of the dialog's content runs — a Settings deep link focuses its anchored
  // heading from one, and a capture after it took that heading (gone once
  // Settings closes) for the trigger.
  useLayoutEffect(() => {
    if (!isOpen) return
    previousFocusRef.current = document.activeElement as HTMLElement
  }, [isOpen])

  // Focus lifecycle, keyed on isOpen ALONE. It must NOT depend on onClose:
  // a parent that passes an inline onClose hands us a new identity every
  // render, and folding that into this effect would tear down and re-run the
  // capture / restore pair mid-interaction — yanking focus back out of the
  // dialog.
  useEffect(() => {
    if (!isOpen) return
    const raf = requestAnimationFrame(() => {
      const container = containerRef.current
      // Something inside already took focus on purpose — a deep link's
      // anchored heading, an editor placing its caret — so leave it there
      // rather than pull it back to the default target.
      const active = document.activeElement
      if (container && active && container.contains(active)) return
      const target = initialFocusRef?.current ?? (container ? getTabbable(container)[0] : undefined)
      target?.focus()
    })
    return () => {
      cancelAnimationFrame(raf)
      // Restore from the CLEANUP rather than an `else` branch on isOpen. The
      // prevailing call pattern is `{open && <Dialog isOpen … />}`, which
      // UNMOUNTS while isOpen is still true — an else-branch restore never runs
      // there and focus is silently dropped onto <body>.
      previousFocusRef.current?.focus()
      previousFocusRef.current = null
    }
  }, [isOpen, containerRef, initialFocusRef])
}
