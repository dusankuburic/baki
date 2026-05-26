import {Minus, Square, X, Settings} from 'lucide-react'
import {getCurrentWindow} from '@tauri-apps/api/window'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'

export default function TitleBar() {
    const document = useFlowStore(s => s.document)
    const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)

    const subflow = document?.subflows.find(s => s.id === selectedSubflowId)
    const breadcrumb = document
        ? [
            document.name,
            ...(subflow ? [subflow.name] : []),
        ]
        : ['PAD Analyzer']

    const win = getCurrentWindow()
    const toggleSettings = useUIStore(s => s.toggleSettings)

    return (
        <div
            className="flex items-center h-8 px-3 border-b border-border-subtle bg-surface-1 flex-shrink-0"
            data-tauri-drag-region
        >
            <span className="text-xs font-medium text-text-tertiary select-none truncate pointer-events-none">
                {breadcrumb.map((segment, i) => (
                    <span key={i}>
                        {i > 0 && <span className="text-text-tertiary mx-1">›</span>}
                        <span className={i === breadcrumb.length - 1 ? 'text-text-secondary' : 'text-text-tertiary'}>
                            {segment}
                        </span>
                    </span>
                ))}
            </span>
            <div className="flex-1 h-full pointer-events-none" />
            <div className="flex items-center gap-1">
                <button
                    onClick={toggleSettings}
                    className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
                    title="Settings (Ctrl+,)"
                >
                    <Settings size={12} />
                </button>
                <button
                    onClick={() => win.minimize()}
                    className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
                >
                    <Minus size={10} />
                </button>
                <button
                    onClick={() => win.toggleMaximize()}
                    className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
                >
                    <Square size={8} />
                </button>
                <button
                    onClick={() => win.close()}
                    className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-[rgba(239,68,68,0.15)] text-text-tertiary hover:text-[#ef4444] transition-colors duration-fast"
                >
                    <X size={10} />
                </button>
            </div>
        </div>
    )
}
