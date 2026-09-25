import {useEffect, useMemo, useRef, useState, type KeyboardEvent} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ChevronDown, Check, Search} from 'lucide-react'
import {healthTier} from '@/lib/scoring'
import type {FacetFlow} from '@/types'

// The flow filter: a button that opens a searchable, keyboard-navigable
// multi-select listbox of every flow in scope, each with its health and its
// finding count under the current filters. Focus returns to the button when
// the list closes.
export function FlowMultiSelect({
  flows,
  selected,
  truncated,
  disabled,
  onChange,
}: {
  flows: FacetFlow[]
  selected: string[]
  truncated: boolean
  disabled?: boolean
  onChange: (ids: string[]) => void
}) {
  const {t} = useTranslation('dashboard')
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(0)
  const rootRef = useRef<HTMLDivElement>(null)
  const buttonRef = useRef<HTMLButtonElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLUListElement>(null)

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return q ? flows.filter(f => f.name.toLowerCase().includes(q)) : flows
  }, [flows, query])
  const selectedSet = useMemo(() => new Set(selected), [selected])

  useEffect(() => {
    if (!open) return
    searchRef.current?.focus()
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false)
    }
    // Escape closes the list wherever focus went (a mouse click on an option
    // keeps it in the search box, but it may also be nowhere).
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      setOpen(false)
      setQuery('')
      setActive(0)
      buttonRef.current?.focus()
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.scrollIntoView({block: 'nearest'})
  }, [active, open])

  const close = (restoreFocus = true) => {
    setOpen(false)
    setQuery('')
    setActive(0)
    if (restoreFocus) buttonRef.current?.focus()
  }

  const toggle = (id: string) => {
    onChange(selectedSet.has(id) ? selected.filter(s => s !== id) : [...selected, id])
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive(i => Math.min(i + 1, Math.max(0, visible.length - 1)))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive(i => Math.max(i - 1, 0))
    } else if (e.key === 'Enter' && visible[active]) {
      e.preventDefault()
      toggle(visible[active].id)
    }
  }

  const label =
    selected.length === 0
      ? t('analytics.filters.flowsAll')
      : t('analytics.filters.flowsSelected', {count: selected.length})

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={buttonRef}
        type="button"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={`${t('analytics.filters.flows')}: ${label}`}
        onClick={() => (open ? close() : setOpen(true))}
        className={clsx(
          'inline-flex h-8 items-center gap-1.5 rounded-md border border-border-default bg-surface-2 px-3 text-sm text-text-primary transition-colors duration-fast hover:bg-surface-3 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500/40',
          disabled && 'cursor-not-allowed opacity-50',
          selected.length > 0 && 'border-brand-500/40',
        )}
      >
        <span>{label}</span>
        <ChevronDown size={14} aria-hidden="true" className="text-text-tertiary" />
      </button>
      {open && (
        <div
          className="absolute left-0 top-full z-20 mt-1 w-80 max-w-[min(20rem,90vw)] rounded-lg border border-border-default bg-surface-2 p-2 shadow-lg"
          onKeyDown={onKeyDown}
        >
          <div className="mb-2 flex h-9 items-center rounded-md border border-border-default bg-surface-1 px-3 transition-colors duration-fast focus-within:border-border-focus">
            <Search size={14} className="mr-2 shrink-0 text-text-tertiary" aria-hidden="true" />
            <input
              ref={searchRef}
              type="search"
              value={query}
              onChange={e => {
                setQuery(e.target.value)
                setActive(0)
              }}
              placeholder={t('analytics.filters.flowsSearch')}
              aria-label={t('analytics.filters.flowsSearch')}
              className="w-full bg-transparent text-sm text-text-primary outline-none placeholder:text-text-disabled"
            />
          </div>
          <ul
            ref={listRef}
            role="listbox"
            aria-multiselectable="true"
            aria-label={t('analytics.filters.flowsList')}
            className="max-h-64 overflow-y-auto scrollbar-thin"
          >
            {visible.length === 0 && (
              <li className="px-2 py-3 text-center text-xs text-text-tertiary">{t('analytics.filters.flowsNone')}</li>
            )}
            {visible.map((f, i) => {
              const isSelected = selectedSet.has(f.id)
              const tier = f.health === null ? null : healthTier(f.health)
              return (
                <li
                  key={f.id}
                  role="option"
                  aria-selected={isSelected}
                  data-index={i}
                  onMouseEnter={() => setActive(i)}
                  // Keep focus in the search box so the arrow keys keep working.
                  onMouseDown={e => e.preventDefault()}
                  onClick={() => toggle(f.id)}
                  className={clsx(
                    'flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-sm',
                    i === active ? 'bg-surface-3' : 'hover:bg-surface-3/60',
                  )}
                >
                  <span
                    aria-hidden="true"
                    className={clsx(
                      'flex h-4 w-4 shrink-0 items-center justify-center rounded border',
                      isSelected ? 'border-brand-500 bg-brand-500 text-brand-foreground' : 'border-border-strong',
                    )}
                  >
                    {isSelected && <Check size={11} />}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-text-primary">{f.name}</span>
                  <span className="shrink-0 text-2xs tabular-nums text-text-tertiary">{f.findings}</span>
                  {tier ? (
                    <span
                      className={clsx(
                        'w-8 shrink-0 rounded px-1 text-center text-2xs font-semibold tabular-nums',
                        tier.bg,
                        tier.text,
                      )}
                    >
                      {f.health}
                    </span>
                  ) : (
                    <span
                      className="w-8 shrink-0 text-center text-2xs text-text-tertiary"
                      title={t('common.notAnalyzed')}
                    >
                      —
                    </span>
                  )}
                </li>
              )
            })}
          </ul>
          <div className="mt-2 flex items-center justify-between gap-2 border-t border-border-subtle pt-2 text-2xs">
            <span className="text-text-tertiary">
              {truncated ? t('analytics.filters.flowsTruncated', {count: flows.length}) : ''}
            </span>
            <span className="flex shrink-0 gap-2">
              <button
                type="button"
                onClick={() => onChange([...new Set([...selected, ...visible.map(f => f.id)])])}
                className="font-medium text-brand-400 hover:text-brand-300"
              >
                {t('analytics.filters.flowsSelectVisible')}
              </button>
              <button
                type="button"
                onClick={() => onChange([])}
                disabled={selected.length === 0}
                className="font-medium text-text-secondary hover:text-text-primary disabled:opacity-50"
              >
                {t('common.clear')}
              </button>
            </span>
          </div>
        </div>
      )}
    </div>
  )
}
