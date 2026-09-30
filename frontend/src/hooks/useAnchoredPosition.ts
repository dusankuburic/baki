import {useCallback, useEffect, useState} from 'react'

export type AnchorSide = 'top' | 'bottom' | 'left' | 'right'
export type AnchorAlign = 'start' | 'center' | 'end'

// Gap between the anchor and the floating element, and the margin kept from
// every viewport edge.
const GAP = 4
const VIEWPORT_MARGIN = 8

// useAnchoredPosition places a position:fixed element (a menu, a popover)
// against its anchor while `open`, clamped into the viewport: below the
// anchor for side 'bottom' (flipped above when it does not fit and the space
// above does), above it otherwise; aligned to the anchor's start, centre or
// end. Extracted from the shared Dropdown so other popups position the same
// way.
//
// The element is position:fixed, so it does NOT follow its anchor when an
// ancestor scrolls — it just floats away. The position is recomputed on
// window resize and on a capture-phase scroll, which catches scrolling
// containers, not only the window.
//
// Returns the viewport coordinates for the element's `top`/`left` style, and
// `update` for callers whose floating content changes size while open.
export function useAnchoredPosition({
  open,
  anchorRef,
  floatingRef,
  side = 'bottom',
  align = 'start',
}: {
  open: boolean
  anchorRef: React.RefObject<HTMLElement | null>
  floatingRef: React.RefObject<HTMLElement | null>
  side?: AnchorSide
  align?: AnchorAlign
}): {top: number; left: number; update: () => void} {
  const [position, setPosition] = useState({top: 0, left: 0})

  const update = useCallback(() => {
    if (!anchorRef.current || !floatingRef.current) return
    const anchorRect = anchorRef.current.getBoundingClientRect()
    const floatingRect = floatingRef.current.getBoundingClientRect()

    let top = side === 'bottom' ? anchorRect.bottom + GAP : anchorRect.top - floatingRect.height - GAP
    let left =
      align === 'start'
        ? anchorRect.left
        : align === 'end'
          ? anchorRect.right - floatingRect.width
          : anchorRect.left + (anchorRect.width - floatingRect.width) / 2

    // Viewport constraints
    if (left + floatingRect.width > window.innerWidth - VIEWPORT_MARGIN)
      left = window.innerWidth - floatingRect.width - VIEWPORT_MARGIN
    if (left < VIEWPORT_MARGIN) left = VIEWPORT_MARGIN

    if (top + floatingRect.height > window.innerHeight - VIEWPORT_MARGIN) {
      // Flip to top if it fits better
      if (side === 'bottom' && anchorRect.top - floatingRect.height > VIEWPORT_MARGIN) {
        top = anchorRect.top - floatingRect.height - GAP
      } else {
        top = window.innerHeight - floatingRect.height - VIEWPORT_MARGIN
      }
    }
    if (top < VIEWPORT_MARGIN) top = VIEWPORT_MARGIN

    setPosition({top, left})
  }, [anchorRef, floatingRef, side, align])

  useEffect(() => {
    if (!open) return
    update()
    window.addEventListener('resize', update)
    window.addEventListener('scroll', update, true)
    return () => {
      window.removeEventListener('resize', update)
      window.removeEventListener('scroll', update, true)
    }
  }, [open, update])

  return {top: position.top, left: position.left, update}
}
