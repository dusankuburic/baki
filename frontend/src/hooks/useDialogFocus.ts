import {useEffect, useLayoutEffect, useRef} from 'react'
import {isImeKeydown} from '@/lib/imeKey'

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

export const FOCUSABLE = `${NATIVELY_FOCUSABLE}, [tabindex]`

export function getTabbable(container: HTMLElement): HTMLElement[] {
  const hasLayout = container.getClientRects().length > 0
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(el => isTabbable(el, hasLayout))
}

function isTabbable(el: HTMLElement, hasLayout: boolean): boolean {
  const tabindex = el.getAttribute('tabindex')
  if (tabindex !== null) {
    const n = Number.parseInt(tabindex, 10)
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

function isDetailsToggle(el: Element): boolean {
  const parent = el.parentElement
  return parent?.localName === 'details' && parent.querySelector(':scope > summary') === el
}

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
  const index = dialogStack.indexOf(entry)
  if (index !== -1) dialogStack.splice(index, 1)
  if (dialogStack.length === 0) document.removeEventListener('keydown', handleDialogKeyDown)
}

function handleDialogKeyDown(e: KeyboardEvent) {
  const top = dialogStack[dialogStack.length - 1]
  if (!top) return
  const {onClose, closeOnEsc, containerRef} = top.latest.current
  if (e.key === 'Escape') {
    if (!closeOnEsc || e.defaultPrevented || isImeKeydown(e)) return
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
  if (!first || !last) return
  const wrapTo = e.shiftKey ? last : first
  if (tabbable.includes(current)) {
    if (current === (e.shiftKey ? first : last)) {
      e.preventDefault()
      wrapTo.focus()
    }
    return
  }
  const hasNext = tabbable.some(el => (e.shiftKey ? follows(current, el) : follows(el, current)))
  if (!hasNext) {
    e.preventDefault()
    wrapTo.focus()
  }
}

function returnFocusIfItLeaves(container: HTMLElement, backwards: boolean) {
  setTimeout(() => {
    const now = document.activeElement
    if (!now || now === document.body || container.contains(now)) return
    if (dialogStack[dialogStack.length - 1]?.latest.current.containerRef.current !== container) return
    const tabbable = getTabbable(container)
    tabbable[backwards ? tabbable.length - 1 : 0]?.focus()
  })
}

function follows(a: Node, b: Node): boolean {
  return (b.compareDocumentPosition(a) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
}

export function useDialogFocus(opts: {
  isOpen: boolean
  onClose: () => void
  closeOnEsc?: boolean
  containerRef: React.RefObject<HTMLElement | null>
  initialFocusRef?: React.RefObject<HTMLElement | null>
}) {
  const {isOpen, onClose, closeOnEsc = true, containerRef, initialFocusRef} = opts
  const previousFocusRef = useRef<HTMLElement | null>(null)

  const latestRef = useRef<DialogProps>({onClose, closeOnEsc, containerRef})
  useLayoutEffect(() => {
    latestRef.current = {onClose, closeOnEsc, containerRef}
  })

  useLayoutEffect(() => {
    if (!isOpen) return
    const entry: DialogEntry = {latest: latestRef}
    pushDialog(entry)
    return () => removeDialog(entry)
  }, [isOpen])

  useLayoutEffect(() => {
    if (!isOpen) return
    previousFocusRef.current = document.activeElement as HTMLElement
  }, [isOpen])

  useEffect(() => {
    if (!isOpen) return
    const raf = requestAnimationFrame(() => {
      const container = containerRef.current
      const active = document.activeElement
      if (container && active && container.contains(active)) return
      const target = initialFocusRef?.current ?? (container ? getTabbable(container)[0] : undefined)
      target?.focus()
    })
    return () => {
      cancelAnimationFrame(raf)
      previousFocusRef.current?.focus()
      previousFocusRef.current = null
    }
  }, [isOpen, containerRef, initialFocusRef])
}
