import {useTranslation} from 'react-i18next'
import {useState, useMemo, useRef, useCallback} from 'react'
import {
  FileText,
  FolderOpen,
  Search,
  ArrowDownAZ,
  ArrowDownWideNarrow,
  Copy,
  ExternalLink,
  RotateCw,
} from 'lucide-react'
import clsx from 'clsx'
import ContextMenu, {type ContextMenuItem} from '@/components/shared/ContextMenu'
import {writeClipboard} from '@/lib/clipboard'
import type {FlowFileInfo} from '@/types'

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

type SortMode = 'name' | 'size'

type FileListProps = {
  files: FlowFileInfo[]
  selectedFilePath: string | null
  folderName: string
  onSelectFile: (path: string) => void
  onRevealFile?: (path: string) => void
  onReloadFile?: (path: string) => void
}

// A PAD flow-export folder is a flat sibling group of subflow .txt files for
// ONE flow (Main.txt + the subflows it calls) — there is no meaningful
// subdirectory nesting here, so this stays a flat, sortable/filterable list
// rather than a directory tree.
export default function FileList({
  files,
  selectedFilePath,
  folderName,
  onSelectFile,
  onRevealFile,
  onReloadFile,
}: FileListProps) {
  const {t} = useTranslation('shell')
  const [filter, setFilter] = useState('')
  const [sortMode, setSortMode] = useState<SortMode>('name')
  const [focusedIndex, setFocusedIndex] = useState(-1)
  const [ctxMenu, setCtxMenu] = useState<{file: FlowFileInfo; x: number; y: number} | null>(null)
  const listRef = useRef<HTMLDivElement>(null)

  const visibleFiles = useMemo(() => {
    const q = filter.trim().toLowerCase()
    const filtered = q ? files.filter(f => f.name.toLowerCase().includes(q)) : files
    const sorted = [...filtered].sort((a, b) => (sortMode === 'size' ? b.size - a.size : a.name.localeCompare(b.name)))
    return sorted
  }, [files, filter, sortMode])

  // focusedIndex can go stale/out-of-range when the filtered/sorted set
  // changes size (e.g. narrowing the filter past the previously-focused
  // row) — clamp it for display and navigation instead of syncing it back
  // with an effect, so an out-of-range value never renders or steps wrong.
  const clampedFocusedIndex =
    visibleFiles.length === 0 ? -1 : Math.min(Math.max(focusedIndex, 0), visibleFiles.length - 1)

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (visibleFiles.length === 0) return
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault()
          setFocusedIndex(Math.min(clampedFocusedIndex + 1, visibleFiles.length - 1))
          break
        case 'ArrowUp':
          e.preventDefault()
          setFocusedIndex(Math.max(clampedFocusedIndex - 1, 0))
          break
        case 'Enter': {
          e.preventDefault()
          const f = visibleFiles[clampedFocusedIndex]
          if (f) onSelectFile(f.path)
          break
        }
      }
    },
    [visibleFiles, clampedFocusedIndex, onSelectFile],
  )

  const menuItems: ContextMenuItem[] = ctxMenu
    ? [
        {
          label: t('files.copyPath'),
          icon: Copy,
          onClick: () => {
            void writeClipboard(ctxMenu.file.path)
          },
        },
        ...(onRevealFile
          ? [
              {
                label: t('files.revealInFileManager'),
                icon: ExternalLink,
                onClick: () => onRevealFile(ctxMenu.file.path),
              },
            ]
          : []),
        ...(onReloadFile
          ? [
              {
                label: t('files.reloadFromDisk'),
                icon: RotateCw,
                onClick: () => onReloadFile(ctxMenu.file.path),
              },
            ]
          : []),
      ]
    : []

  return (
    <div className="border-b border-border-subtle">
      <div className="flex items-center gap-2 px-3 py-2 border-b border-border-subtle">
        <FolderOpen size={14} className="text-brand-400 flex-shrink-0" />
        <span className="text-xs font-medium text-text-secondary truncate">{folderName}</span>
        <span className="text-2xs text-text-tertiary ml-auto flex-shrink-0 tabular-nums">
          {files.length} file{files.length !== 1 ? 's' : ''}
        </span>
      </div>

      {files.length > 4 && (
        <div className="flex items-center gap-1.5 px-3 py-1.5 border-b border-border-subtle">
          <div className="flex items-center gap-1.5 flex-1 min-w-0 h-6 px-2 bg-surface-2 border border-border-subtle rounded">
            <Search size={11} className="text-text-tertiary flex-shrink-0" />
            <input
              value={filter}
              onChange={e => setFilter(e.target.value)}
              placeholder={t('files.filterPlaceholder')}
              className="flex-1 min-w-0 bg-transparent text-2xs text-text-primary placeholder:text-text-disabled outline-none"
            />
          </div>
          <button
            onClick={() => setSortMode(m => (m === 'name' ? 'size' : 'name'))}
            className="w-6 h-6 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast flex-shrink-0"
            title={
              sortMode === 'name' ? 'Sorted by name — click to sort by size' : 'Sorted by size — click to sort by name'
            }
          >
            {sortMode === 'name' ? <ArrowDownAZ size={12} /> : <ArrowDownWideNarrow size={12} />}
          </button>
        </div>
      )}

      <div
        ref={listRef}
        className="max-h-64 overflow-y-auto outline-none"
        tabIndex={0}
        onKeyDown={handleKeyDown}
        role="listbox"
        aria-label={t('files.listAria')}
      >
        {visibleFiles.length === 0 ? (
          <div className="px-3 py-4 text-2xs text-text-tertiary text-center">No files match "{filter}"</div>
        ) : (
          visibleFiles.map((file, idx) => {
            const isSelected = file.path === selectedFilePath
            const isFocused = idx === clampedFocusedIndex
            const displayName = file.name.replace(/\.txt$/i, '')
            return (
              <button
                key={file.path}
                role="option"
                aria-selected={isSelected}
                className={clsx(
                  'w-full flex items-center gap-2 px-3 py-1.5 text-left transition-colors duration-fast',
                  isSelected ? 'bg-brand-500/10 text-brand-400' : 'text-text-secondary hover:bg-surface-3',
                  isFocused && !isSelected && 'ring-1 ring-inset ring-border-default',
                )}
                onClick={() => {
                  setFocusedIndex(idx)
                  onSelectFile(file.path)
                }}
                onContextMenu={e => {
                  e.preventDefault()
                  setFocusedIndex(idx)
                  setCtxMenu({file, x: e.clientX, y: e.clientY})
                }}
              >
                <FileText
                  size={14}
                  className={clsx('flex-shrink-0', isSelected ? 'text-brand-400' : 'text-text-tertiary')}
                />
                <span className="text-sm truncate flex-1">{displayName}</span>
                <span className="text-2xs text-text-tertiary flex-shrink-0 tabular-nums">{formatSize(file.size)}</span>
              </button>
            )
          })
        )}
      </div>

      {ctxMenu && <ContextMenu x={ctxMenu.x} y={ctxMenu.y} items={menuItems} onClose={() => setCtxMenu(null)} />}
    </div>
  )
}
