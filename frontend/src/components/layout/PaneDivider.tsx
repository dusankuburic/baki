import {useCallback, useRef, useState} from 'react'
import clsx from 'clsx'

interface PaneDividerProps {
  // Pixels since the previous move (+ = right). Keyboard steps arrive through
  // the same callback, so callers clamp in one place and commit once.
  onDrag: (delta: number) => void
  onResizeEnd: () => void
  onDoubleClick: () => void
  // Accessible name and value, supplied (translated) by the caller — this
  // file carries no copy of its own.
  label: string
  value?: number
  min?: number
  max?: number
  // Home / End. Omitted where a jump makes no sense (editor groups).
  onJump?: (to: 'min' | 'max') => void
}

const KEY_STEP = 16
const KEY_STEP_LARGE = 64

// PaneDivider is the splitter between panes, and a real control: visible (a
// hairline that lights up with a grip on hover or focus), operable by pointer
// (mouse, touch, pen — with cancel handling) and by keyboard (the APG
// window-splitter pattern: arrows move, Home/End jump, Enter resets).
export default function PaneDivider({
  onDrag,
  onResizeEnd,
  onDoubleClick,
  label,
  value,
  min,
  max,
  onJump,
}: PaneDividerProps) {
  const draggingRef = useRef(false)
  const [dragging, setDragging] = useState(false)
  const lastX = useRef(0)

  const handlePointerDown = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault()
    draggingRef.current = true
    setDragging(true)
    lastX.current = e.clientX
    // The control itself, not e.target: the hit-area child is what is under
    // the pointer most of the time.
    e.currentTarget.setPointerCapture?.(e.pointerId)
  }, [])

  const handlePointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!draggingRef.current) return
      const delta = e.clientX - lastX.current
      lastX.current = e.clientX
      onDrag(delta)
    },
    [onDrag],
  )

  // pointerup, pointercancel and a lost capture all end the drag exactly once;
  // a drag that never ended used to leave the pane stuck at the last width
  // without a commit.
  const endDrag = useCallback(() => {
    if (!draggingRef.current) return
    draggingRef.current = false
    setDragging(false)
    onResizeEnd()
  }, [onResizeEnd])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      const step = e.shiftKey ? KEY_STEP_LARGE : KEY_STEP
      switch (e.key) {
        case 'ArrowLeft':
          onDrag(-step)
          onResizeEnd()
          break
        case 'ArrowRight':
          onDrag(step)
          onResizeEnd()
          break
        case 'Home':
          if (!onJump) return
          onJump('min')
          break
        case 'End':
          if (!onJump) return
          onJump('max')
          break
        case 'Enter':
          onDoubleClick()
          break
        default:
          return
      }
      e.preventDefault()
    },
    [onDrag, onResizeEnd, onDoubleClick, onJump],
  )

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label={label}
      aria-valuenow={value}
      aria-valuemin={min}
      aria-valuemax={max}
      tabIndex={0}
      className={clsx(
        'group relative w-[3px] shrink-0 cursor-col-resize select-none touch-none outline-none print:hidden',
        'focus-visible:ring-2 focus-visible:ring-brand-500/60',
      )}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      onLostPointerCapture={endDrag}
      onDoubleClick={onDoubleClick}
      onKeyDown={handleKeyDown}
    >
      {/* Hairline: always visible, brand-tinted while hovered, focused or dragged. */}
      <div
        className={clsx(
          'absolute inset-y-0 left-1/2 w-px -translate-x-1/2 transition-colors duration-fast',
          dragging
            ? 'bg-brand-500'
            : 'bg-border-subtle group-hover:bg-brand-500/60 group-focus-visible:bg-brand-500/60',
        )}
      />
      {/* Grip */}
      <div
        className={clsx(
          'absolute left-1/2 top-1/2 h-8 w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-brand-500 transition-opacity duration-fast',
          dragging ? 'opacity-100' : 'opacity-0 delay-150 group-hover:opacity-100 group-focus-visible:opacity-100',
        )}
      />
      {/* A wider hit area than the 3px slot. */}
      <div className="absolute inset-y-0 -left-1 -right-1" />
    </div>
  )
}
