import {useEffect, useRef} from 'react'

export function useScrollIntoView(selectedId: string | null) {
    const containerRef = useRef<HTMLDivElement>(null)

    useEffect(() => {
        if (!selectedId) return
        const el = document.querySelector(`[data-block-id="${selectedId}"]`)
        if (!el) return

        const rect = el.getBoundingClientRect()
        const viewHeight = window.innerHeight || document.documentElement.clientHeight
        if (rect.top < 0 || rect.bottom > viewHeight) {
            el.scrollIntoView({behavior: 'smooth', block: 'center'})
        }
    }, [selectedId])

    return containerRef
}
