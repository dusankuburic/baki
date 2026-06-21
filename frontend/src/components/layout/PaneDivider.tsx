import {useCallback, useRef, useState, useEffect} from 'react'

interface PaneDividerProps {
    onDrag: (delta: number) => void
    onResizeEnd: () => void
    onDoubleClick: () => void
}

export default function PaneDivider({onDrag, onResizeEnd, onDoubleClick}: PaneDividerProps) {
    const draggingRef = useRef(false)
    const [dragging, setDragging] = useState(false)
    const lastX = useRef(0)
    const [hovered, setHovered] = useState(false)
    const hoverTimer = useRef<ReturnType<typeof setTimeout>>()

    const handlePointerDown = useCallback((e: React.PointerEvent) => {
        e.preventDefault()
        draggingRef.current = true
        setDragging(true)
        lastX.current = e.clientX
        ;(e.target as HTMLElement).setPointerCapture(e.pointerId)
    }, [])

    const handlePointerMove = useCallback((e: React.PointerEvent) => {
        if (!draggingRef.current) return
        const delta = e.clientX - lastX.current
        lastX.current = e.clientX
        onDrag(delta)
    }, [onDrag])

    const handlePointerUp = useCallback(() => {
        if (draggingRef.current) {
            draggingRef.current = false
            setDragging(false)
            onResizeEnd()
        }
    }, [onResizeEnd])

    const handleMouseEnter = useCallback(() => {
        hoverTimer.current = setTimeout(() => setHovered(true), 200)
    }, [])

    const handleMouseLeave = useCallback(() => {
        clearTimeout(hoverTimer.current)
        setHovered(false)
    }, [])

    useEffect(() => {
        return () => clearTimeout(hoverTimer.current)
    }, [])

    const isActive = dragging || hovered

    return (
        <div
            className="w-[3px] flex-shrink-0 cursor-col-resize group relative bg-transparent print:hidden"
            style={{
                backgroundColor: isActive ? 'color-mix(in srgb, var(--brand-500) 30%, transparent)' : 'transparent',
                transition: dragging ? 'none' : 'background-color 120ms var(--ease-out)',
            }}
            onMouseEnter={handleMouseEnter}
            onMouseLeave={handleMouseLeave}
            onPointerDown={handlePointerDown}
            onPointerMove={handlePointerMove}
            onPointerUp={handlePointerUp}
            onDoubleClick={onDoubleClick}
        >
            <div className="absolute inset-y-0 -left-[3px] -right-[3px]" />
        </div>
    )
}
