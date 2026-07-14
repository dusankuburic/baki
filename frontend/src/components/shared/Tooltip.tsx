import {useState, useRef, useCallback, useEffect} from 'react'
import clsx from 'clsx'

type TooltipProps = {
  content: React.ReactNode
  side?: 'top' | 'bottom' | 'left' | 'right'
  shortcut?: string
  delay?: number
  children: React.ReactNode
}

export default function Tooltip({content, side = 'top', shortcut, delay = 600, children}: TooltipProps) {
  const [visible, setVisible] = useState(false)
  const [position, setPosition] = useState({top: 0, left: 0})
  const wrapperRef = useRef<HTMLSpanElement>(null)
  const tooltipRef = useRef<HTMLDivElement>(null)
  const timeoutRef = useRef<ReturnType<typeof setTimeout>>()

  const updatePosition = useCallback(() => {
    if (!wrapperRef.current || !tooltipRef.current) return
    const triggerRect = wrapperRef.current.getBoundingClientRect()
    const tooltipRect = tooltipRef.current.getBoundingClientRect()
    const gap = 8

    let top = 0
    let left = 0

    switch (side) {
      case 'top':
        top = triggerRect.top - tooltipRect.height - gap
        left = triggerRect.left + (triggerRect.width - tooltipRect.width) / 2
        break
      case 'bottom':
        top = triggerRect.bottom + gap
        left = triggerRect.left + (triggerRect.width - tooltipRect.width) / 2
        break
      case 'left':
        top = triggerRect.top + (triggerRect.height - tooltipRect.height) / 2
        left = triggerRect.left - tooltipRect.width - gap
        break
      case 'right':
        top = triggerRect.top + (triggerRect.height - tooltipRect.height) / 2
        left = triggerRect.right + gap
        break
    }

    if (left < 4) left = 4
    if (left + tooltipRect.width > window.innerWidth - 4) left = window.innerWidth - tooltipRect.width - 4
    if (top < 4) top = 4
    if (top + tooltipRect.height > window.innerHeight - 4) top = window.innerHeight - tooltipRect.height - 4

    setPosition({top, left})
  }, [side])

  const show = useCallback(() => {
    timeoutRef.current = setTimeout(() => {
      setVisible(true)
    }, delay)
  }, [delay])

  const hide = useCallback(() => {
    clearTimeout(timeoutRef.current)
    setVisible(false)
  }, [])

  useEffect(() => {
    if (visible) {
      updatePosition()
    }
  }, [visible, updatePosition])

  useEffect(() => {
    return () => clearTimeout(timeoutRef.current)
  }, [])

  return (
    <>
      {visible && (
        <div
          ref={tooltipRef}
          className={clsx(
            'fixed z-tooltip bg-surface-4 text-text-primary text-xs px-2 py-1 rounded-md shadow-md animate-fade-in',
            'pointer-events-none max-w-xs',
          )}
          style={{top: position.top, left: position.left}}
          role="tooltip"
        >
          <span>{content}</span>
          {shortcut && <span className="ml-2 text-text-tertiary">{shortcut}</span>}
        </div>
      )}
      <span
        ref={wrapperRef}
        onMouseEnter={show}
        onMouseLeave={hide}
        onFocus={show}
        onBlur={hide}
        className="inline-flex"
      >
        {children}
      </span>
    </>
  )
}
