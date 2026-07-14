import clsx from 'clsx'
import {ChevronDown} from 'lucide-react'
import {useState, useRef, useEffect} from 'react'
import type {ModelDetail} from '@/types'

interface Props {
  models: ModelDetail[]
  selected: string
  onSelect: (modelId: string) => void
}

export default function ModelSelector({models, selected, onSelect}: Props) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [])

  const current = models.find(m => m.id === selected)

  if (models.length <= 1) return null

  return (
    <div ref={ref} className="relative mx-3">
      <button
        className="flex items-center gap-1.5 px-2 py-1 rounded-md hover:bg-surface-2 text-xs text-text-tertiary transition-colors"
        onClick={() => setOpen(!open)}
      >
        <span className="truncate">{current?.displayName || selected}</span>
        <ChevronDown size={12} className="shrink-0" />
      </button>
      {open && (
        <div className="absolute top-full left-0 mt-1 bg-surface-1 border border-border-default rounded-lg shadow-lg z-overlay py-1 min-w-[220px]">
          {models.map(m => (
            <button
              key={m.id}
              className={clsx(
                'flex flex-col gap-0.5 px-3 py-2 text-left w-full hover:bg-surface-2 transition-colors',
                m.id === selected && 'text-brand-400',
              )}
              onClick={() => {
                onSelect(m.id)
                setOpen(false)
              }}
            >
              <span className="text-xs font-medium">{m.displayName}</span>
              <span className="text-2xs text-text-tertiary">
                {m.contextLimit / 1000}k ctx
                {m.inputCostPerM > 0 && ` \u00b7 $${m.inputCostPerM}/$${m.outputCostPerM} per 1M`}
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
