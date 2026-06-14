import {FileText, FolderOpen} from 'lucide-react'
import clsx from 'clsx'
import type {FlowFileInfo} from '@/types'

function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

type FileListProps = {
    files: FlowFileInfo[]
    selectedFilePath: string | null
    folderName: string
    onSelectFile: (path: string) => void
}

export default function FileList({files, selectedFilePath, folderName, onSelectFile}: FileListProps) {
    return (
        <div className="border-b border-border-subtle">
            <div className="flex items-center gap-2 px-3 py-2 border-b border-border-subtle">
                <FolderOpen size={14} className="text-brand-400 flex-shrink-0" />
                <span className="text-xs font-medium text-text-secondary truncate">{folderName}</span>
                <span className="text-2xs text-text-tertiary ml-auto flex-shrink-0">{files.length} file{files.length !== 1 ? 's' : ''}</span>
            </div>
            <div className="max-h-48 overflow-y-auto">
                {files.map(file => {
                    const isSelected = file.path === selectedFilePath
                    const displayName = file.name.replace(/\.txt$/i, '')
                    return (
                        <button
                            key={file.path}
                            className={clsx(
                                'w-full flex items-center gap-2 px-3 py-1.5 text-left transition-colors duration-fast',
                                isSelected ? 'bg-brand-500/10 text-brand-400' : 'text-text-secondary hover:bg-surface-3',
                            )}
                            onClick={() => onSelectFile(file.path)}
                        >
                            <FileText size={14} className={clsx('flex-shrink-0', isSelected ? 'text-brand-400' : 'text-text-tertiary')} />
                            <span className="text-sm truncate flex-1">{displayName}</span>
                            <span className="text-2xs text-text-tertiary flex-shrink-0">{formatSize(file.size)}</span>
                        </button>
                    )
                })}
            </div>
        </div>
    )
}
