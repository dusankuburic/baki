import clsx from 'clsx'
import {Check, ChevronDown} from 'lucide-react'
import {useState, useRef, useEffect} from 'react'
import type {ProviderID} from '@/types'

interface ProviderOption {
  id: ProviderID
  name: string
  configured: boolean
  authType: string
}

interface Props {
  providers: ProviderOption[]
  selected: ProviderID
  onSelect: (id: ProviderID) => void
}

export default function ProviderSelector({providers, selected, onSelect}: Props) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [])

  const current = providers.find(p => p.id === selected)

  return (
    <div ref={ref} className="relative mx-3">
      <button
        className="flex items-center gap-2 px-2 py-1.5 rounded-md hover:bg-surface-2 text-sm text-text-secondary transition-colors w-full"
        onClick={() => setOpen(!open)}
      >
        <span className="truncate">{current?.name || 'Select provider'}</span>
        <ChevronDown size={14} className="shrink-0" />
      </button>
      {open && (
        <div className="absolute top-full left-0 right-0 mt-1 bg-surface-1 border border-border-default rounded-lg shadow-lg z-overlay py-1">
          {providers.map(p => (
            <button
              key={p.id}
              className={clsx(
                'flex items-center gap-2 px-3 py-2 text-sm w-full text-left hover:bg-surface-2 transition-colors',
                p.id === selected && 'text-brand-400',
              )}
              onClick={() => {
                onSelect(p.id)
                setOpen(false)
              }}
              disabled={!p.configured}
            >
              <span className="flex-1 truncate">{p.name}</span>
              {p.id === selected && <Check size={14} />}
              {!p.configured && <span className="text-2xs text-text-tertiary">Not configured</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
