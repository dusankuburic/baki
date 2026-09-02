import {useState, useMemo, useRef, useEffect} from 'react'
import clsx from 'clsx'
import {Kbd} from '@/components/shared'
import {useListNavigation} from '@/hooks/useListNavigation'
import {useDialogFocus} from '@/hooks/useDialogFocus'

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
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const paletteRef = useRef<HTMLDivElement>(null)
  // Tab focus trap + focus restoration to the trigger on close. Esc is left to
  // useListNavigation's onClose (closeOnEsc:false here avoids a double-close).
  useDialogFocus({isOpen, onClose, closeOnEsc: false, containerRef: paletteRef})

  const grouped = useMemo(() => {
    const lower = query.toLowerCase()
    const filtered = query
      ? commands.filter(c => c.label.toLowerCase().includes(lower) || c.section.toLowerCase().includes(lower))
      : commands

    const groups: Record<string, Command[]> = {}
    for (const cmd of filtered) {
      ;(groups[cmd.section] ??= []).push(cmd)
    }
    return groups
  }, [commands, query])

  const {flatItems, indexById} = useMemo(() => {
    const items: Command[] = []
    for (const cmds of Object.values(grouped)) {
      items.push(...cmds)
    }
    // Precompute the flat index per command id so the render below is pure
    // (a render-time counter would depend on Object.values iteration order).
    const idxById = new Map<string, number>()
    items.forEach((c, i) => idxById.set(c.id, i))
    return {flatItems: items, indexById: idxById}
  }, [grouped])

  const {activeIndex, setActiveIndex, handleKeyDown} = useListNavigation({
    count: flatItems.length,
    onSelect: i => {
      flatItems[i]?.onSelect()
      onClose()
    },
    onClose,
  })

  useEffect(() => {
    if (isOpen) {
      setQuery('')
      setActiveIndex(0)
      requestAnimationFrame(() => inputRef.current?.focus())
    }
  }, [isOpen, setActiveIndex])

  useEffect(() => {
    setActiveIndex(0)
  }, [query, setActiveIndex])

  useEffect(() => {
    const active = listRef.current?.querySelector('[data-active="true"]')
    active?.scrollIntoView({block: 'nearest'})
  }, [activeIndex])

  if (!isOpen) return null

  return (
    <div className="fixed inset-0 z-modal flex items-start justify-center pt-[20vh]" onClick={onClose}>
      <div className="absolute inset-0 bg-surface-overlay backdrop-blur-sm" />
      <div
        ref={paletteRef}
        className="relative w-full max-w-[640px] max-md:max-w-none max-md:h-full max-md:rounded-none bg-surface-1 border border-border-default rounded-xl shadow-xl overflow-hidden animate-palette"
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
                {cmds.map(cmd => {
                  const idx = indexById.get(cmd.id) ?? -1
                  return (
                    <div
                      key={cmd.id}
                      role="option"
                      aria-selected={idx === activeIndex}
                      data-active={idx === activeIndex}
                      className={clsx(
                        'flex items-center px-4 py-2 cursor-pointer text-sm transition-colors duration-fast',
                        idx === activeIndex
                          ? 'bg-surface-3 text-text-primary'
                          : 'text-text-secondary hover:bg-surface-2',
                      )}
                      onClick={() => {
                        cmd.onSelect()
                        onClose()
                      }}
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
            <div className="py-8 text-center text-sm text-text-tertiary">No commands found</div>
          )}
        </div>
      </div>
    </div>
  )
}
