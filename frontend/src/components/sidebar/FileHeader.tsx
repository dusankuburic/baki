import {useTranslation} from 'react-i18next'
import {useState, useCallback, useRef} from 'react'
import {FolderOpen, ChevronDown, FilePlus, Loader2} from 'lucide-react'
import clsx from 'clsx'
import Tooltip from '@/components/shared/Tooltip'
import RecentFilesMenu from './RecentFilesMenu'
import type {FlowDocument, RecentFile} from '@/types'

type FileHeaderProps = {
  document: FlowDocument | null
  recentFiles: RecentFile[]
  isLoading?: boolean
  onNewFlow?: () => void
  onOpenFile: () => void
  onOpenFolder: () => void
  onLoadRecent: (path: string) => void
  onRemoveRecent: (path: string) => void
  onClearRecent: () => void
}

export default function FileHeader({
  document,
  recentFiles,
  isLoading = false,
  onNewFlow,
  onOpenFile,
  onOpenFolder,
  onLoadRecent,
  onRemoveRecent,
  onClearRecent,
}: FileHeaderProps) {
  const {t} = useTranslation('shell')
  const [menuOpen, setMenuOpen] = useState(false)
  // The recents menu is anchored to the whole header row (matching its
  // previous left-0/right-0 width), not just the chevron that opens it.
  const headerRef = useRef<HTMLDivElement>(null)

  const handleRecentSelect = useCallback(
    (path: string) => {
      setMenuOpen(false)
      onLoadRecent(path)
    },
    [onLoadRecent],
  )

  if (!document) {
    return (
      <div className="flex items-center h-12 px-3 border-b border-border-subtle gap-2">
        {onNewFlow && (
          <button
            onClick={onNewFlow}
            disabled={isLoading}
            className="flex items-center gap-1.5 h-8 px-3 text-sm font-medium text-text-secondary bg-surface-3 border border-border-default rounded-md hover:bg-surface-4 transition-colors duration-fast disabled:opacity-50 disabled:cursor-wait shrink-0"
            title={t('files.newTitle')}
          >
            <FilePlus size={14} />
            <span>{t('files.new')}</span>
          </button>
        )}
        <button
          onClick={onOpenFile}
          disabled={isLoading}
          className="flex items-center gap-2 flex-1 h-8 px-3 text-sm font-medium text-brand-400 bg-brand-500/10 border border-brand-500/30 rounded-md hover:bg-brand-500/15 transition-colors duration-fast disabled:opacity-50 disabled:cursor-wait"
        >
          {isLoading ? <Loader2 size={14} className="animate-spin" /> : <FolderOpen size={14} />}
          <span>{isLoading ? t('files.loading') : t('files.openFile')}</span>
        </button>
        <button
          onClick={onOpenFolder}
          disabled={isLoading}
          className="flex items-center gap-2 h-8 px-3 text-sm font-medium text-text-secondary bg-surface-3 border border-border-default rounded-md hover:bg-surface-4 transition-colors duration-fast disabled:opacity-50 disabled:cursor-wait"
        >
          {isLoading ? <Loader2 size={14} className="animate-spin" /> : <FolderOpen size={14} />}
          <span>{t('files.folder')}</span>
        </button>
      </div>
    )
  }

  return (
    <div ref={headerRef} className="flex items-center h-12 px-3 border-b border-border-subtle gap-2">
      <Tooltip content={document.filePath}>
        <span className="text-sm font-medium text-text-primary truncate flex-1 select-none">{document.name}</span>
      </Tooltip>
      <button
        onClick={onOpenFolder}
        className="w-6 h-6 flex items-center justify-center rounded-sm text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
        title={t('files.openFolder')}
        aria-label={t('files.openFolder')}
      >
        <FolderOpen size={14} />
      </button>
      <button
        onClick={() => setMenuOpen(v => !v)}
        className={clsx(
          'w-6 h-6 flex items-center justify-center rounded-sm text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast',
          menuOpen && 'text-text-secondary bg-surface-3',
        )}
      >
        <ChevronDown size={14} />
      </button>
      {menuOpen && (
        <RecentFilesMenu
          files={recentFiles}
          anchorRef={headerRef}
          onSelect={handleRecentSelect}
          onRemove={onRemoveRecent}
          onClear={onClearRecent}
          onClose={() => setMenuOpen(false)}
        />
      )}
    </div>
  )
}
