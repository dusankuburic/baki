import {List, Network, Map, History, Minus, Plus, Maximize2, Download, Expand, ChevronLeft, ChevronRight, MapPin, Flame} from 'lucide-react'
import SegmentedControl from '@/components/shared/SegmentedControl'
import IconButton from '@/components/shared/IconButton'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {exportApi} from '@/api'
import {useToast} from '@/components/shared/Toast'

export default function MainPaneToolbar() {
    const mainPaneView = useUIStore(s => s.mainPaneView)
    const setMainPaneView = useUIStore(s => s.setMainPaneView)
    const complexityMode = useUIStore(s => s.complexityMode)
    const toggleComplexityMode = useUIStore(s => s.toggleComplexityMode)
    const graphZoom = useUIStore(s => s.graphZoom)
    const setGraphZoom = useUIStore(s => s.setGraphZoom)
    const setActiveDiff = useUIStore(s => s.setActiveDiff)
    const document = useFlowStore(s => s.document)
    const groups = useFlowStore(s => s.groups)
    const focusedGroupIndex = useFlowStore(s => s.focusedGroupIndex)
    const navigationHistory = useFlowStore(s => s.navigationHistory)
    const historyIndex = useFlowStore(s => s.historyIndex)
    const goBack = useFlowStore(s => s.goBack)
    const goForward = useFlowStore(s => s.goForward)

    const activeTabId = groups[focusedGroupIndex]?.activeTabId ?? null
    const subflow = activeTabId
        ? document?.subflows.find(s => s.id === activeTabId)
        : document?.subflows[0]
    const breadcrumb = document
        ? [document.name, ...(subflow && subflow.name !== 'Main' ? [subflow.name] : [])]
        : []

    const toast = useToast()

    const handleExport = async (format: 'pdf' | 'markdown') => {
        try {
            const fn = format === 'pdf' ? exportApi.exportPDF : exportApi.exportMarkdown
            const path = await fn()
            if (path) {
                toast.success(`Exported to ${path}`)
            }
        } catch (e) {
            toast.error('Export failed: ' + (e as Error).message)
        }
    }

    const handleCompare = async () => {
        try {
            const path = await exportApi.pickFile('Select Old Version for Comparison')
            if (!path) return
            
            toast.info('Comparing flows...')
            const diff = await exportApi.compareCurrentWith(path)
            if (diff) {
                setActiveDiff(diff as any)
                setMainPaneView('diff')
                toast.success('Comparison complete')
            }
        } catch (e) {
            toast.error('Comparison failed: ' + (e as Error).message)
        }
    }

    return (
        <div className="flex items-center h-12 px-4 border-b border-border-default bg-surface-1 gap-4">
            <div className="flex items-center gap-1">
                <IconButton 
                    icon={ChevronLeft} 
                    size="sm" 
                    label="Go Back" 
                    disabled={historyIndex <= 0}
                    onClick={goBack} 
                />
                <IconButton 
                    icon={ChevronRight} 
                    size="sm" 
                    label="Go Forward" 
                    disabled={historyIndex >= navigationHistory.length - 1}
                    onClick={goForward} 
                />
            </div>

            <div className="w-px h-6 bg-border-subtle mx-1" />

            <SegmentedControl
                value={mainPaneView}
                onChange={setMainPaneView}
                options={[
                    {value: 'block' as const, label: 'Block', icon: List},
                    {value: 'graph' as const, label: 'Graph', icon: Network},
                    {value: 'local-map' as const, label: 'Local Map', icon: MapPin},
                    {value: 'map' as const, label: 'Map', icon: Map},
                    {value: 'diff' as const, label: 'Diff', icon: History},
                ]}
                size="sm"
            />

            <div className="flex-1 flex items-center justify-center">
                {breadcrumb.length > 0 && mainPaneView !== 'diff' && (
                    <div className="flex items-center gap-1 text-sm">
                        {breadcrumb.map((segment, i) => (
                            <span key={i} className="flex items-center gap-1">
                                {i > 0 && <span className="text-text-tertiary">›</span>}
                                <span className={i === breadcrumb.length - 1 ? 'text-text-primary font-medium' : 'text-text-tertiary'}>
                                    {segment}
                                </span>
                            </span>
                        ))}
                    </div>
                )}
                {mainPaneView === 'diff' && (
                    <div className="text-sm font-medium text-brand-500 flex items-center gap-2">
                        <History size={16} />
                        Version Comparison
                    </div>
                )}
            </div>

            <div className="flex items-center gap-1">
                {(mainPaneView === 'graph' || mainPaneView === 'map' || mainPaneView === 'local-map') && (
                    <>
                        <IconButton icon={Minus} size="sm" label="Zoom out" onClick={() => setGraphZoom(Math.max(0.25, graphZoom - 0.1))} />
                        <button
                            className="w-12 h-7 text-xs text-text-tertiary tabular-nums text-center hover:text-text-secondary"
                            onClick={() => setGraphZoom(1)}
                        >
                            {Math.round(graphZoom * 100)}%
                        </button>
                        <IconButton icon={Plus} size="sm" label="Zoom in" onClick={() => setGraphZoom(Math.min(3, graphZoom + 0.1))} />
                        <IconButton icon={Maximize2} size="sm" label="Fit to screen" onClick={() => {
                            window.dispatchEvent(new Event('graph:fit'))
                        }} />
                    </>
                )}
                {mainPaneView === 'diff' && (
                    <IconButton 
                        icon={Plus} 
                        size="sm" 
                        label="New Comparison" 
                        onClick={handleCompare} 
                    />
                )}
                {mainPaneView === 'block' && (
                    <IconButton 
                        icon={Flame} 
                        size="sm" 
                        label="Complexity Map" 
                        onClick={toggleComplexityMode}
                        className={complexityMode ? 'text-semantic-warning bg-semantic-warning/10' : ''}
                    />
                )}
                <IconButton icon={Expand} size="sm" label="Fullscreen" onClick={() => { try { window.document.documentElement.requestFullscreen() } catch (_e) { /* fullscreen not supported */ } }} />
                <IconButton icon={Download} size="sm" label="Export PDF" onClick={() => handleExport('pdf')} />
            </div>
        </div>
    )
}
