import {useState, useRef, useEffect, useCallback} from 'react'
import clsx from 'clsx'
import Kbd from './Kbd'

type DropdownItem =
    | { type: 'item'; label: string; icon?: React.ComponentType<{size?: number; className?: string}>; shortcut?: string; onSelect: () => void; danger?: boolean; disabled?: boolean }
    | { type: 'separator' }
    | { type: 'group'; label: string; items: DropdownItem[] }

type DropdownProps = {
    trigger: React.ReactElement
    items: DropdownItem[]
    side?: 'top' | 'bottom' | 'left' | 'right'
    align?: 'start' | 'center' | 'end'
    className?: string
}

export type {DropdownItem}

export default function Dropdown({trigger, items, side = 'bottom', align = 'start', className}: DropdownProps) {
    const [open, setOpen] = useState(false)
    const [activeIndex, setActiveIndex] = useState(-1)
    const triggerRef = useRef<HTMLDivElement>(null)
    const menuRef = useRef<HTMLDivElement>(null)
    const [position, setPosition] = useState({top: 0, left: 0})

    const flattenedItems = flattenItems(items) as FlattenedDropdownItem[]

    const updatePosition = useCallback(() => {
        if (!triggerRef.current || !menuRef.current) return
        const triggerRect = triggerRef.current.getBoundingClientRect()
        const menuRect = menuRef.current.getBoundingClientRect()

        let top = side === 'bottom' ? triggerRect.bottom + 4 : triggerRect.top - menuRect.height - 4
        let left = align === 'start' ? triggerRect.left
          : align === 'end' ? triggerRect.right - menuRect.width
          : triggerRect.left + (triggerRect.width - menuRect.width) / 2

        // Viewport constraints
        if (left + menuRect.width > window.innerWidth - 8) left = window.innerWidth - menuRect.width - 8
        if (left < 8) left = 8

        if (top + menuRect.height > window.innerHeight - 8) {
            // Flip to top if it fits better
            if (side === 'bottom' && triggerRect.top - menuRect.height > 8) {
                top = triggerRect.top - menuRect.height - 4
            } else {
                top = window.innerHeight - menuRect.height - 8
            }
        }
        if (top < 8) top = 8

        setPosition({top, left})
    }, [side, align])

    useEffect(() => {
        if (open) {
            updatePosition()
            setActiveIndex(-1)
        }
    }, [open, updatePosition])

    useEffect(() => {
        if (!open) return
        const handler = (e: MouseEvent) => {
            if (!triggerRef.current?.contains(e.target as Node) && !menuRef.current?.contains(e.target as Node)) {
                setOpen(false)
            }
        }
        document.addEventListener('mousedown', handler)
        return () => document.removeEventListener('mousedown', handler)
    }, [open])

    const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
        if (!open) return
        const selectable = flattenedItems.filter(i => i.type === 'item' && !i.disabled)
        if (e.key === 'ArrowDown') {
            e.preventDefault()
            setActiveIndex(prev => {
                const idx = selectable.findIndex(i => i._index === prev)
                return idx < selectable.length - 1 ? selectable[idx + 1]._index : selectable[0]._index
            })
        } else if (e.key === 'ArrowUp') {
            e.preventDefault()
            setActiveIndex(prev => {
                const idx = selectable.findIndex(i => i._index === prev)
                return idx > 0 ? selectable[idx - 1]._index : selectable[selectable.length - 1]._index
            })
        } else if (e.key === 'Enter' && activeIndex >= 0) {
            const item = flattenedItems[activeIndex]
            if (item?.type === 'item' && !item.disabled) {
                item.onSelect()
                setOpen(false)
            }
        } else if (e.key === 'Escape') {
            setOpen(false)
        }
    }, [open, activeIndex, flattenedItems])

    return (
        <div className={clsx('inline-block', className)} onKeyDown={handleKeyDown}>
            <div ref={triggerRef} onClick={() => setOpen(!open)}>
                {trigger}
            </div>
            {open && (
                <div
                    ref={menuRef}
                    className="fixed z-overlay bg-surface-2 border border-border-default rounded-lg shadow-lg py-1 min-w-[180px] animate-fade-in"
                    style={{top: position.top, left: position.left}}
                    role="menu"
                >
                    {renderItems(items, activeIndex, () => setOpen(false))}
                </div>
            )}
        </div>
    )
}

type FlattenedDropdownItem = DropdownItem & {_index: number}

function flattenItems(items: DropdownItem[]): FlattenedDropdownItem[] {
    const result: FlattenedDropdownItem[] = []
    let idx = 0
    function walk(list: DropdownItem[]) {
        for (const item of list) {
            if (item.type === 'group') {
                walk(item.items)
            } else {
                result.push({...item, _index: idx})
            }
            idx++
        }
    }
    walk(items)
    return result
}

function renderItems(items: (DropdownItem & {_index?: number})[], activeIndex: number, close: () => void): React.ReactNode {
    return items.map((item, i) => {
        if (item.type === 'separator') {
            return <div key={i} className="my-1 mx-2 h-px bg-border-subtle" />
        }
        if (item.type === 'group') {
            return (
                <div key={i}>
                    <div className="text-2xs uppercase tracking-wider text-text-tertiary px-2 py-1">
                        {item.label}
                    </div>
                    {renderItems(item.items, activeIndex, close)}
                </div>
            )
        }
        const Icon = item.icon
        const isActive = item._index === activeIndex
        return (
            <button
                key={i}
                className={clsx(
                    'w-full flex items-center h-8 px-2 text-sm text-left transition-colors duration-fast',
                    isActive ? 'bg-surface-3 border-l-2 border-brand-500' : 'border-l-2 border-transparent',
                    item.danger ? 'text-semantic-error hover:bg-semantic-error/10' : 'text-text-primary hover:bg-surface-3',
                    item.disabled && 'opacity-50 cursor-not-allowed pointer-events-none'
                )}
                onClick={() => {
                    if (!item.disabled) {
                        item.onSelect()
                        close()
                    }
                }}
                role="menuitem"
            >
                {Icon && <Icon size={16} className="mr-2 text-text-tertiary flex-shrink-0" />}
                <span className="flex-1">{item.label}</span>
                {item.shortcut && <Kbd keys={[item.shortcut]} size="xs" className="ml-4" />}
            </button>
        )
    })
}
