import {useState, useEffect, useMemo} from 'react'
import {flowApi} from '@/api'
import {logger} from '@/lib/logger'
import type {SourceFileInfo} from '@/types/domain'
import {FileText} from 'lucide-react'

interface Props {
  query: string
  onSelect: (file: string) => void
  onClose: () => void
}

export default function FileAutocomplete({query, onSelect}: Props) {
  const [files, setFiles] = useState<SourceFileInfo[]>([])

  useEffect(() => {
    flowApi.getSourceFiles().then(setFiles).catch((err) => { logger.warn('Failed to load source files', err) })
  }, [])

  const filtered = useMemo(() => {
    const q = query.toLowerCase()
    return files.filter(f => f.filename.toLowerCase().includes(q))
  }, [files, query])

  if (filtered.length === 0) return null

  return (
    <div className="absolute bottom-full mb-2 w-64 bg-surface-2 border border-border-default rounded-lg shadow-xl overflow-hidden z-50">
      <div className="px-2 py-1.5 text-xs font-semibold text-text-tertiary uppercase tracking-wider border-b border-border-default">
        Select a file
      </div>
      <div className="max-h-48 overflow-y-auto">
        {filtered.map(file => (
          <button
            key={file.filename}
            className="w-full flex items-center gap-2 px-3 py-2 text-sm text-text-secondary hover:bg-surface-3 hover:text-text-primary transition-colors"
            onClick={() => onSelect(file.filename)}
          >
            <FileText size={14} />
            {file.filename}
          </button>
        ))}
      </div>
    </div>
  )
}
