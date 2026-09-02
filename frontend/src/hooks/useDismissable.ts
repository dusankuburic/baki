import {useEffect, useRef} from 'react'

// useDismissable wires the standard popover dismissal contract (U1.5):
// close on outside mousedown AND on Escape, while `active` is true. Extracted
// from FindingTriageMenu so every popover (saved views, undo ring, triage
// submenus) shares one behavior instead of each hand-rolling a subset —
// the toolbar popovers previously stayed open until their button was
// re-clicked.
//
// Returns a ref to attach to the popover's outermost element; clicks inside
// it never dismiss.
export function useDismissable(active: boolean, onDismiss: () => void) {
  const ref = useRef<HTMLDivElement | null>(null)
  const dismissRef = useRef(onDismiss)
  // Keep the ref's latest-callback semantics without touching refs during
  // render (React Compiler lint): the sync-less assignment runs as an effect
  // after commit, which still precedes any user-driven dismissal event.
  useEffect(() => {
    dismissRef.current = onDismiss
  })

  useEffect(() => {
    if (!active) return
    const onMouseDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) dismissRef.current()
    }
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dismissRef.current()
    }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [active])

  return ref
}
