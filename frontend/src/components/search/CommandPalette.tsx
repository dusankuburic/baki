import {useState, useCallback, useMemo, useRef, useEffect} from 'react'
import clsx from 'clsx'
import {Kbd} from '@/components/shared'

type Command = {
    id: string
    label: string
    section: string
    shortcut?: string[]
    onSelect: () => void
}

type CommandPaletteProps = {
    isOpen: boolean
    onClose: () => void
    commands?: Command[]
}

export default function CommandPalette({isOpen, onClose, commands = []}: CommandPaletteProps) {
    const [query, setQuery] = useState('')
    const [activeIndex, setActiveIndex] = useState(0)
    const inputRef = useRef<HTMLInputElement>(null)
    const listRef = useRef<HTMLDivElement>(null)

    useEffect(() => {
        if (isOpen) {
            setQuery('')
            setActiveIndex(0)
            requestAnimationFrame(() => inputRef.current?.focus())
        }
    }, [isOpen])

    const grouped = useMemo(() => {
        const lower = query.toLowerCase()
        const filtered = query
            ? commands.filter(c =>
                c.label.toLowerCase().includes(lower) || c.section.toLowerCase().includes(lower)
            )
            : commands

        const groups: Record<string, Command[]> = {}
        for (const cmd of filtered) {
            ;(groups[cmd.section] ??= []).push(cmd)
        }
        return groups
    }, [commands, query])

    const flatItems = useMemo(() => {
        const items: Command[] = []
        for (const cmds of Object.values(grouped)) {
            items.push(...cmds)
        }
        return items
    }, [grouped])

    useEffect(() => {
        setActiveIndex(0)
    }, [query])

    useEffect(() => {
        const active = listRef.current?.querySelector('[data-active="true"]')
        active?.scrollIntoView({block: 'nearest'})
    }, [activeIndex])

    const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
        if (e.key === 'ArrowDown') {
            e.preventDefault()
            setActiveIndex(prev => Math.min(prev + 1, flatItems.length - 1))
        } else if (e.key === 'ArrowUp') {
            e.preventDefault()
            setActiveIndex(prev => Math.max(prev - 1, 0))
        } else if (e.key === 'Enter' && flatItems[activeIndex]) {
            e.preventDefault()
            flatItems[activeIndex].onSelect()
            onClose()
        } else if (e.key === 'Escape') {
            e.preventDefault()
            onClose()
        }
    }, [flatItems, activeIndex, onClose])

    if (!isOpen) return null

    let itemIndex = -1

    return (
        <div className="fixed inset-0 z-modal flex items-start justify-center pt-[20vh]" onClick={onClose}>
            <div
                className="absolute inset-0 bg-surface-overlay backdrop-blur-sm"
            />
            <div
                className="relative w-full max-w-[640px] bg-surface-1 border border-border-default rounded-xl shadow-xl overflow-hidden animate-palette"
                onClick={e => e.stopPropagation()}
            >
                <div className="flex items-center px-4 border-b border-border-subtle">
                    <span className="text-text-tertiary mr-2 text-sm">&#x1F50D;</span>
                    <input
                        ref={inputRef}
                        type="text"
                        value={query}
                        onChange={e => setQuery(e.target.value)}
                        onKeyDown={handleKeyDown}
                        placeholder="Type a command..."
                        className="flex-1 bg-transparent py-3 text-sm outline-none text-text-primary placeholder:text-text-disabled"
                    />
                </div>
                <div ref={listRef} className="max-h-64 overflow-y-auto py-1">
                    {Object.keys(grouped).length > 0 ? (
                        Object.entries(grouped).map(([section, cmds]) => (
                            <div key={section}>
                                <div className="text-2xs font-medium uppercase tracking-wider text-text-tertiary px-4 py-1.5">
                                    {section}
                                </div>
                                {cmds.map((cmd) => {
                                    itemIndex++
                                    const idx = itemIndex
                                    return (
                                        <div
                                            key={cmd.id}
                                            data-active={idx === activeIndex}
                                            className={clsx(
                                                'flex items-center px-4 py-2 cursor-pointer text-sm transition-colors duration-fast',
                                                idx === activeIndex ? 'bg-surface-3 text-text-primary' : 'text-text-secondary hover:bg-surface-2'
                                            )}
                                            onClick={() => {cmd.onSelect(); onClose()}}
                                            onMouseEnter={() => setActiveIndex(idx)}
                                        >
                                            <span className="flex-1">{cmd.label}</span>
                                            {cmd.shortcut && <Kbd keys={cmd.shortcut} size="xs" className="ml-4" />}
                                        </div>
                                    )
                                })}
                            </div>
                        ))
                    ) : (
                        <div className="py-8 text-center text-sm text-text-tertiary">
                            No commands found
                        </div>
                    )}
                </div>
            </div>
        </div>
    )
}
