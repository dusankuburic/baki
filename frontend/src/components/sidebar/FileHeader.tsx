import {useState, useCallback} from 'react'
import {FolderOpen, ChevronDown} from 'lucide-react'
import clsx from 'clsx'
import Tooltip from '@/components/shared/Tooltip'
import RecentFilesMenu from './RecentFilesMenu'
import type {FlowDocument, RecentFile} from '@/types/domain'

type FileHeaderProps = {
    document: FlowDocument | null
    recentFiles: RecentFile[]
    onOpenFile: () => void
    onOpenFolder: () => void
    onLoadRecent: (path: string) => void
    onRemoveRecent: (path: string) => void
    onClearRecent: () => void
}

export default function FileHeader({
    document,
    recentFiles,
    onOpenFile,
    onOpenFolder,
    onLoadRecent,
    onRemoveRecent,
    onClearRecent,
}: FileHeaderProps) {
    const [menuOpen, setMenuOpen] = useState(false)

    const handleRecentSelect = useCallback((path: string) => {
        setMenuOpen(false)
        onLoadRecent(path)
    }, [onLoadRecent])

    if (!document) {
        return (
            <div className="flex items-center h-12 px-3 border-b border-border-subtle gap-2">
                <button
                    onClick={onOpenFile}
                    className="flex items-center gap-2 flex-1 h-8 px-3 text-sm font-medium text-brand-400 bg-brand-500/10 border border-brand-500/30 rounded-md hover:bg-brand-500/15 transition-colors duration-fast"
                >
                    <FolderOpen size={14} />
                    <span>Open file</span>
                </button>
                <button
                    onClick={onOpenFolder}
                    className="flex items-center gap-2 h-8 px-3 text-sm font-medium text-text-secondary bg-surface-3 border border-border-default rounded-md hover:bg-surface-4 transition-colors duration-fast"
                >
                    <FolderOpen size={14} />
                    <span>Folder</span>
                </button>
            </div>
        )
    }

    return (
        <div className="flex items-center h-12 px-3 border-b border-border-subtle gap-2">
            <Tooltip content={document.filePath}>
                <span className="text-sm font-medium text-text-primary truncate flex-1 select-none">
                    {document.name}
                </span>
            </Tooltip>
            <button
                onClick={onOpenFolder}
                className="w-6 h-6 flex items-center justify-center rounded-sm text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
                title="Open folder"
            >
                <FolderOpen size={14} />
            </button>
            <div className="relative">
                <button
                    onClick={() => setMenuOpen(v => !v)}
                    className={clsx(
                        'w-6 h-6 flex items-center justify-center rounded-sm text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast',
                        menuOpen && 'text-text-secondary bg-surface-3'
                    )}
                >
                    <ChevronDown size={14} />
                </button>
                {menuOpen && (
                    <RecentFilesMenu
                        files={recentFiles}
                        onSelect={handleRecentSelect}
                        onRemove={onRemoveRecent}
                        onClear={onClearRecent}
                        onClose={() => setMenuOpen(false)}
                    />
                )}
            </div>
        </div>
    )
}
