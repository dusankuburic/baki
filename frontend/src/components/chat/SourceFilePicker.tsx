import {useState, useRef, useEffect, useCallback} from 'react'
import {FileText, ChevronDown, Check, Files} from 'lucide-react'
import clsx from 'clsx'
import type {SourceFileInfo} from '@/types/domain'
import {Portal} from '../shared'

interface Props {
  files: SourceFileInfo[]
  selected: string[]
  onSelectionChange: (filenames: string[]) => void
}

export default function SourceFilePicker({files, selected, onSelectionChange}: Props) {
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState({top: 0, left: 0, width: 0})
  const ref = useRef<HTMLDivElement>(null)

  const updatePosition = useCallback(() => {
    if (ref.current) {
      const rect = ref.current.getBoundingClientRect()
      setPos({top: rect.bottom + 4, left: rect.left, width: rect.width})
    }
  }, [])

  useEffect(() => {
    if (open) {
      updatePosition()
      window.addEventListener('resize', updatePosition)
      window.addEventListener('scroll', updatePosition, true)
    }
    return () => {
      window.removeEventListener('resize', updatePosition)
      window.removeEventListener('scroll', updatePosition, true)
    }
  }, [open, updatePosition])

  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  if (files.length === 0) return null

  const allSelected = files.length > 0 && selected.length === files.length
  const noneSelected = selected.length === 0
  const sel = new Set(selected)

  const toggleAll = () => {
    onSelectionChange(allSelected ? [] : files.map(f => f.filename))
  }

  const toggleFile = (filename: string) => {
    if (sel.has(filename)) {
      onSelectionChange(selected.filter(s => s !== filename))
    } else {
      onSelectionChange([...selected, filename])
    }
  }

  const label = noneSelected
    ? 'No files selected'
    : allSelected
      ? `All ${files.length} files`
      : `${selected.length} of ${files.length} files`

  return (
    <div ref={ref} className="relative px-3">
      <button
        className={clsx(
          'flex items-center gap-2 w-full px-2.5 py-1.5 rounded-lg text-xs transition-colors border',
          open ? 'bg-surface-2 border-border-default' : 'hover:bg-surface-2 border-transparent'
        )}
        onClick={() => setOpen(!open)}
      >
        <FileText size={12} className="text-text-tertiary shrink-0" />
        <span className={clsx('truncate', selected.length > 0 ? 'text-text-secondary font-medium' : 'text-text-tertiary')}>
          {label}
        </span>
        {selected.length > 0 && (
          <span className="ml-auto text-2xs text-brand-400 shrink-0">
            {selected.length}
          </span>
        )}
        <ChevronDown size={12} className={clsx('shrink-0 text-text-tertiary transition-transform', open && 'rotate-180')} />
      </button>

      {open && (
        <Portal>
          <div 
            className="fixed bg-surface-1 border border-border-default rounded-lg shadow-lg z-overlay py-1 animate-fade-in max-h-[280px] overflow-y-auto"
            style={{top: pos.top, left: pos.left + 12, width: pos.width - 24}}
          >
            <button
              className={clsx(
                'flex items-center gap-2.5 px-3 py-2 text-xs w-full text-left hover:bg-surface-2 transition-colors border-b border-border-subtle',
                allSelected && 'text-brand-400'
              )}
              onClick={toggleAll}
            >
              <Files size={13} className="shrink-0" />
              <span className="flex-1 font-medium">Select all files</span>
              {allSelected && <Check size={13} className="shrink-0" />}
            </button>

            {files.map(f => {
              const checked = sel.has(f.filename)
              return (
                <button
                  key={f.filename}
                  className={clsx(
                    'flex items-center gap-2.5 px-3 py-1.5 text-xs w-full text-left hover:bg-surface-2 transition-colors',
                    checked && 'text-brand-400'
                  )}
                  onClick={() => toggleFile(f.filename)}
                >
                  <div className={clsx(
                    'w-3.5 h-3.5 rounded border flex items-center justify-center shrink-0 transition-colors',
                    checked ? 'bg-brand-500 border-brand-500' : 'border-border-default'
                  )}>
                    {checked && <Check size={10} className="text-white" />}
                  </div>
                  <div className="flex-1 min-w-0">
                    <span className="block truncate font-medium">{f.subflowName}</span>
                    <span className="block text-2xs text-text-tertiary truncate">{f.filename}</span>
                  </div>
                  <span className="text-2xs text-text-tertiary shrink-0">
                    {f.blockCount} blocks
                  </span>
                </button>
              )
            })}
          </div>
        </Portal>
      )}
    </div>
  )
}
