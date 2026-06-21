import {useCallback, useMemo, useRef, Fragment, lazy, Suspense, useState, memo} from 'react'
import {X, FolderOpen, XCircle, MinusSquare, AlertTriangle} from 'lucide-react'
import {BlockView, MainPaneToolbar} from '@/components/flow'
import ParseErrorsBanner from '@/components/flow/ParseErrorsBanner'
import {Spinner, ErrorBoundary} from '@/components/shared'
import {UserProfile} from '@/components/auth/UserProfile'
import {AdminDashboard} from '@/components/admin/AdminDashboard'
import ContextMenu, {type ContextMenuItem} from '@/components/shared/ContextMenu'
import Breadcrumbs from './Breadcrumbs'

// GraphView pulls in cytoscape (~100KB+); keep it out of the entry chunk like
// its lazy siblings below.
const GraphView = lazy(() => import('@/components/graph/GraphView'))
const ExecutionGraphView = lazy(() => import('@/components/flow/ExecutionGraphView'))
const RegressionDiffView = lazy(() => import('@/components/flow/RegressionDiffView'))
const AnalyticsDashboard = lazy(() => import('@/components/dashboard/AnalyticsDashboard'))
// HomeDashboard pulls in recharts (~100KB+); lazy-load keeps it out of the entry chunk.
const HomeDashboard = lazy(() => import('@/components/dashboard/HomeDashboard'))
// LibraryWorkspace pulls in the multi-pane browse experience and its helpers;
// lazy keeps it out of the entry chunk and only loads when the user opens it.
const LibraryWorkspace = lazy(() => import('@/components/library/LibraryWorkspace'))
const PortfolioView = lazy(() => import('@/components/dashboard/PortfolioView'))
import PaneDivider from '@/components/layout/PaneDivider'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {useEditorStore, type EditorGroup} from '@/stores/editorStore'
import type {FlowDocument} from '@/types'

export default function MainPane() {
    // All hooks must run unconditionally on every render (Rules of Hooks).
    // The profile/admin/empty views are handled by early returns AFTER the hooks.
    const mainPaneView = useUIStore(s => s.mainPaneView)
    const document = useFlowStore(s => s.document)
    const parseError = useFlowStore(s => s.parseError)
    const groups = useEditorStore(s => s.groups)
    const focusedGroupIndex = useEditorStore(s => s.focusedGroupIndex)
    const groupWidths = useEditorStore(s => s.groupWidths)
    const focusGroup = useEditorStore(s => s.focusGroup)
    const openInGroup = useEditorStore(s => s.openInGroup)
    const closeTab = useEditorStore(s => s.closeTab)
    const closeAllTabs = useEditorStore(s => s.closeAllTabs)
    const closeOtherTabs = useEditorStore(s => s.closeOtherTabs)
    const closeGroup = useEditorStore(s => s.closeGroup)
    const moveTabToGroup = useEditorStore(s => s.moveTabToGroup)
    const setGroupWidths = useEditorStore(s => s.setGroupWidths)
    const containerRef = useRef<HTMLDivElement>(null)

    const widths = useMemo(() => {
        if (groups.length <= 1) return [1]
        if (groupWidths.length === groups.length) return groupWidths
        return groups.map(() => 1 / groups.length)
    }, [groups.length, groupWidths])

    const handleColumnDrag = useCallback((leftIndex: number, delta: number) => {
        if (groups.length < 2) return
        const containerWidth = containerRef.current?.clientWidth ?? 1
        const fraction = delta / containerWidth
        const newWidths = [...widths]
        const left = newWidths[leftIndex] + fraction
        const right = newWidths[leftIndex + 1] - fraction
        if (left < 0.1 || right < 0.1) return
        newWidths[leftIndex] = left
        newWidths[leftIndex + 1] = right
        setGroupWidths(newWidths)
    }, [widths, groups.length, setGroupWidths])

    const handleResetDivider = useCallback(() => {
        setGroupWidths(groups.map(() => 1 / groups.length))
    }, [groups.length, setGroupWidths])

    if (mainPaneView === 'profile') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <MainPaneToolbar />
                <div className="flex-1 overflow-y-auto p-4">
                    <UserProfile />
                </div>
            </div>
        )
    }

    if (mainPaneView === 'admin') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <MainPaneToolbar />
                <div className="flex-1 overflow-y-auto p-4">
                    <AdminDashboard />
                </div>
            </div>
        )
    }

    if (mainPaneView === 'dashboard') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <MainPaneToolbar />
                <div className="flex-1 overflow-y-auto p-4">
                    <Suspense fallback={<Spinner />}>
                        <AnalyticsDashboard />
                    </Suspense>
                </div>
            </div>
        )
    }

    if (mainPaneView === 'home') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <MainPaneToolbar />
                <div className="flex-1 overflow-hidden">
                    <Suspense fallback={<Spinner />}>
                        <HomeDashboard />
                    </Suspense>
                </div>
            </div>
        )
    }

    if (mainPaneView === 'library') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <Suspense fallback={<Spinner />}>
                    <LibraryWorkspace />
                </Suspense>
            </div>
        )
    }

    if (mainPaneView === 'portfolio') {
        return (
            <div className="flex flex-col h-full bg-surface-1">
                <MainPaneToolbar />
                <div className="flex-1 overflow-hidden">
                    <Suspense fallback={<Spinner />}>
                        <PortfolioView />
                    </Suspense>
                </div>
            </div>
        )
    }

    if (!document) {
        return (
            <div className="flex flex-col h-full">
                <MainPaneToolbar />
                <div className="flex-1 flex flex-col items-center justify-center">
                    {parseError ? (
                        <>
                            <div className="w-16 h-16 rounded-full bg-red-500/10 flex items-center justify-center mb-4">
                                <AlertTriangle size={28} className="text-red-400" />
                            </div>
                            <div className="text-xl font-medium text-text-secondary mb-2">Failed to load flow</div>
                            <div className="text-sm text-red-400/90 max-w-md text-center break-words">{parseError}</div>
                            <div className="text-sm text-text-tertiary mt-2">Try another file from the sidebar</div>
                        </>
                    ) : (
                        <>
                            <div className="w-16 h-16 rounded-full bg-surface-2 flex items-center justify-center mb-4">
                                <FolderOpen size={28} className="text-text-tertiary" />
                            </div>
                            <div className="text-xl font-medium text-text-secondary mb-2">Open a flow to begin</div>
                            <div className="text-sm text-text-tertiary">Choose from the sidebar or drag a file here</div>
                        </>
                    )}
                </div>
            </div>
        )
    }

    return (
        <div className="flex flex-col h-full">
            <MainPaneToolbar />
            <ParseErrorsBanner />
            <div ref={containerRef} className="flex-1 flex overflow-hidden">
                {groups.map((group, gi) => (
                    <Fragment key={`group-${gi}`}>
                        {gi > 0 && (
                            <PaneDivider
                                onDrag={(delta) => handleColumnDrag(gi - 1, delta)}
                                onResizeEnd={() => {}}
                                onDoubleClick={handleResetDivider}
                            />
                        )}
                        <div
                            className={`flex flex-col overflow-hidden ${gi === focusedGroupIndex ? '' : 'opacity-90'}`}
                            style={{flex: `${widths[gi]} 0 0`, minWidth: 200}}
                            onMouseDown={(e) => {
                                if (e.target === e.currentTarget || (e.target as HTMLElement).closest('.group-header')) {
                                    if (gi !== focusedGroupIndex) focusGroup(gi)
                                }
                            }}
                            onDragOver={(e) => {
                                e.preventDefault()
                                e.dataTransfer.dropEffect = 'move'
                            }}
                            onDrop={(e) => {
                                const data = e.dataTransfer.getData('application/tab-move')
                                if (!data) return
                                try {
                                    const {fromGroup, subflowId} = JSON.parse(data)
                                    if (fromGroup !== gi) moveTabToGroup(fromGroup, subflowId, gi)
                                } catch { /* ignore invalid drop data */ }
                            }}
                        >
                            <GroupTabStrip
                                document={document}
                                group={group}
                                groupIndex={gi}
                                isFocused={gi === focusedGroupIndex}
                                totalGroups={groups.length}
                                onSelectTab={(subflowId) => openInGroup(subflowId, gi)}
                                onCloseTab={(subflowId) => closeTab(gi, subflowId)}
                                onCloseAllTabs={() => closeAllTabs(gi)}
                                onCloseOtherTabs={(subflowId) => closeOtherTabs(gi, subflowId)}
                                onCloseGroup={() => closeGroup(gi)}
                            />
                            {group.activeTabId && gi === focusedGroupIndex && <Breadcrumbs />}
                            <div className="flex-1 flex flex-col overflow-hidden">
                                {mainPaneView === 'diff' ? (
                                    gi === 0 && (
                                        <ErrorBoundary><Suspense fallback={<Spinner />}>
                                            <RegressionDiffView key="global-diff" />
                                        </Suspense></ErrorBoundary>
                                    )
                                ) : group.activeTabId && (
                                    mainPaneView === 'block' ? (
                                        <div key={group.activeTabId} className="flex-1 overflow-y-auto">
                                            <BlockView subflowId={group.activeTabId} />
                                        </div>
                                    ) : mainPaneView === 'graph' ? (
                                        <ErrorBoundary><Suspense fallback={<Spinner />}>
                                            <GraphView key={group.activeTabId} subflowId={group.activeTabId} />
                                        </Suspense></ErrorBoundary>
                                    ) : mainPaneView === 'local-map' ? (
                                        <ErrorBoundary><Suspense fallback={<Spinner />}>
                                            <ExecutionGraphView key={`local-map-${group.activeTabId}`} subflowId={group.activeTabId} />
                                        </Suspense></ErrorBoundary>
                                    ) : mainPaneView === 'map' ? (
                                        <ErrorBoundary><Suspense fallback={<Spinner />}>
                                            <ExecutionGraphView key="global-map" />
                                        </Suspense></ErrorBoundary>
                                    ) : null
                                )}
                            </div>
                        </div>
                    </Fragment>
                ))}
            </div>
        </div>
    )
}

const GroupTabStrip = memo(function GroupTabStrip({
    document,
    group,
    groupIndex,
    isFocused,
    totalGroups,
    onSelectTab,
    onCloseTab,
    onCloseAllTabs,
    onCloseOtherTabs,
    onCloseGroup,
}: {
    document: FlowDocument
    group: EditorGroup
    groupIndex: number
    isFocused: boolean
    totalGroups: number
    onSelectTab: (subflowId: string) => void
    onCloseTab: (subflowId: string) => void
    onCloseAllTabs: () => void
    onCloseOtherTabs: (subflowId: string) => void
    onCloseGroup: () => void
}) {
    const [menuPos, setMenuPos] = useState<{x: number, y: number, tabId: string} | null>(null)
    const subflowMap = useMemo(() => {
        const m = new Map<string, typeof document.subflows[0]>()
        for (const sf of document.subflows) m.set(sf.id, sf)
        return m
    }, [document])
    if (group.tabs.length === 0) return null

    return (
        <div className={`group-header flex items-center border-b flex-shrink-0 overflow-hidden ${
            isFocused ? 'border-brand-500/30 bg-surface-0' : 'border-border-default bg-surface-1'
        }`}>
            <div className="flex-1 flex items-center overflow-x-auto">
                {group.tabs.map(tabId => {
                    const sf = subflowMap.get(tabId)
                    if (!sf) return null
                    const isActive = tabId === group.activeTabId
                    
                    const handleContextMenu = (e: React.MouseEvent) => {
                        e.preventDefault()
                        setMenuPos({x: e.clientX, y: e.clientY, tabId})
                    }

                    const contextItems: ContextMenuItem[] = [
                        {
                            label: 'Close',
                            icon: X,
                            onClick: () => onCloseTab(tabId)
                        },
                        {
                            label: 'Close Others',
                            icon: MinusSquare,
                            onClick: () => onCloseOtherTabs(tabId)
                        },
                        {
                            label: 'Close All',
                            icon: XCircle,
                            onClick: () => onCloseAllTabs()
                        }
                    ]

                    return (
                        <Fragment key={tabId}>
                            <button
                                draggable
                                onDragStart={(e) => {
                                    e.dataTransfer.setData('application/tab-move', JSON.stringify({fromGroup: groupIndex, subflowId: tabId}))
                                    e.dataTransfer.effectAllowed = 'move'
                                }}
                                onContextMenu={handleContextMenu}
                                className={`group/tab flex items-center gap-1 px-2.5 h-8 text-xs border-r border-border-subtle whitespace-nowrap transition-colors flex-shrink-0 ${
                                    isActive && isFocused
                                        ? 'bg-surface-0 text-text-primary font-medium border-t-2 border-t-brand-500'
                                        : isActive
                                        ? 'bg-surface-1 text-text-primary font-medium border-t-2 border-t-brand-400/50'
                                        : 'text-text-secondary hover:bg-surface-2 border-t-2 border-t-transparent'
                                }`}
                                onClick={() => onSelectTab(tabId)}
                            >
                                <span className="truncate max-w-[120px]">{sf.sourceFile ? sf.sourceFile.replace(/\.txt$/i, '') : sf.name}</span>
                                <span
                                    className="inline-flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 opacity-0 group-hover/tab:opacity-100 transition-opacity flex-shrink-0"
                                    onClick={(e) => {
                                        e.stopPropagation()
                                        onCloseTab(tabId)
                                    }}
                                >
                                    <X size={11} />
                                </span>
                            </button>
                            {menuPos && menuPos.tabId === tabId && (
                                <ContextMenu
                                    x={menuPos.x}
                                    y={menuPos.y}
                                    onClose={() => setMenuPos(null)}
                                    items={contextItems}
                                />
                            )}
                        </Fragment>
                    )
                })}
            </div>
            {totalGroups > 1 && (
                <button
                    className="flex items-center justify-center w-6 h-8 text-text-tertiary hover:text-text-secondary hover:bg-surface-2 transition-colors flex-shrink-0"
                    onClick={(e) => {
                        e.stopPropagation()
                        onCloseGroup()
                    }}
                    title="Close group"
                >
                    <X size={14} />
                </button>
            )}
        </div>
    )
})
