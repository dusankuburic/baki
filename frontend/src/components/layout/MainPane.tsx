import {Fragment, lazy, Suspense, memo, useState, useMemo, useEffect} from 'react'
import {X, FolderOpen, XCircle, MinusSquare, AlertTriangle, FlaskConical, HelpCircle, Code2} from 'lucide-react'
import {BlockView, MainPaneToolbar} from '@/components/flow'
import SourceEditor from '@/components/flow/SourceEditor'
import ParseErrorsBanner from '@/components/flow/ParseErrorsBanner'
import {Spinner, ErrorBoundary, useToast} from '@/components/shared'
import ContextMenu, {type ContextMenuItem} from '@/components/shared/ContextMenu'
import Breadcrumbs from './Breadcrumbs'
import {SystemViewRouter, isSystemView} from './SystemViewRouter'
import PaneDivider from '@/components/layout/PaneDivider'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi} from '@/api'
import {SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES} from '@/data/sampleFlow'
import type {EditorGroup} from '@/stores/editorStore'
import type {FlowDocument} from '@/types'
import {useEditorGroups} from '@/hooks/useEditorGroups'

// GraphView pulls in cytoscape (~100KB+); keep it out of the entry chunk like
// its lazy siblings below.
const GraphView = lazy(() => import('@/components/graph/GraphView'))
const ExecutionGraphView = lazy(() => import('@/components/flow/ExecutionGraphView'))
const RegressionDiffView = lazy(() => import('@/components/flow/RegressionDiffView'))

// MainPane is a thin router: system views (profile/admin/dashboards/library/
// portfolio) delegate to SystemViewRouter; everything else is the flow editor,
// which owns the document-dependent split-pane groups.
export default function MainPane() {
  const mainPaneView = useUIStore(s => s.mainPaneView)
  if (isSystemView(mainPaneView)) {
    return <SystemViewRouter view={mainPaneView} />
  }
  return <FlowEditorPane mainPaneView={mainPaneView} />
}

// FlowEditorPane renders the no-document empty/error state, or the split editor
// groups for the loaded document. Editor-group state and divider math come from
// the useEditorGroups hook.
function FlowEditorPane({mainPaneView}: {mainPaneView: string}) {
  const document = useFlowStore(s => s.document)
  const parseError = useFlowStore(s => s.parseError)
  const setDocument = useFlowStore(s => s.setDocument)
  const toast = useToast()
  const editor = useEditorGroups()
  const [loadingSample, setLoadingSample] = useState(false)
  const [showHelp, setShowHelp] = useState(false)
  const [showSource, setShowSource] = useState(false)

  // Keep the flow store's selected subflow in sync with the focused group's
  // active tab, whatever changed it — tab click, closing a tab (a neighbor
  // activates), dragging a tab between groups, closing a group. Breadcrumbs,
  // the sidebar highlight and the inspector all follow selectedSubflowId, so
  // without this they keep showing the previous subflow. The echo through
  // selectSubflow → openInGroup terminates immediately (openInGroup no-ops
  // when the tab is already active in the focused group).
  useEffect(() => {
    const activeId = editor.groups[editor.focusedGroupIndex]?.activeTabId
    if (activeId && activeId !== useFlowStore.getState().selectedSubflowId) {
      useFlowStore.getState().selectSubflow(activeId)
    }
  }, [editor.groups, editor.focusedGroupIndex])

  // Open the bundled sample flow so a first-run user sees a real analysis
  // without having to export their own flow first. Posts the embedded PAD
  // text through the normal upload path (parsed server-side), then loads it.
  const handleOpenSample = async () => {
    setLoadingSample(true)
    try {
      const doc = await flowApi.uploadFlow(SAMPLE_FLOW_NAME, SAMPLE_FLOW_FILES)
      if (doc) {
        setDocument(doc)
      } else {
        toast.error('Could not load the sample flow')
      }
    } catch (err) {
      toast.error('Could not load the sample flow', {description: String(err)})
    } finally {
      setLoadingSample(false)
    }
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
              <div className="text-sm text-text-tertiary mb-5">Choose from the sidebar or drag a file here</div>
              <button
                onClick={handleOpenSample}
                disabled={loadingSample}
                className="flex items-center gap-2 px-4 py-2 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium shadow-lg shadow-brand-500/20 hover:bg-brand-600 transition-colors disabled:opacity-50"
              >
                {loadingSample ? <Spinner size={14} /> : <FlaskConical size={16} />}
                {loadingSample ? 'Loading sample…' : 'Try a sample flow'}
              </button>
              <button
                onClick={() => setShowHelp(s => !s)}
                className="mt-4 flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary transition-colors"
              >
                <HelpCircle size={12} />
                How do I export a flow from Power Automate Desktop?
              </button>
              {showHelp && (
                <div className="mt-3 max-w-md text-xs text-text-tertiary bg-surface-2 border border-border-subtle rounded-lg p-4 leading-relaxed">
                  <ol className="list-decimal list-inside space-y-1">
                    <li>
                      Open your flow in the <strong>Power Automate Desktop</strong> designer.
                    </li>
                    <li>Select the actions you want (or the whole flow via the canvas).</li>
                    <li>
                      Right-click → <strong>Copy</strong> (or <code>Ctrl+C</code>), then paste into a <code>.txt</code>{' '}
                      file — or use the designer's <strong>export</strong> option.
                    </li>
                    <li>
                      Save the file and open it here with <strong>Open file</strong>, or drag it onto the window.
                    </li>
                  </ol>
                  <p className="mt-2 text-text-tertiary/80">
                    The analyzer reads PAD's text export format; a folder of subflow <code>.txt</code> files works too.
                  </p>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    )
  }

  const {groups, focusedGroupIndex, widths} = editor

  if (showSource && document) {
    return (
      <div className="flex flex-col h-full">
        <div className="relative">
          <MainPaneToolbar />
          <div className="absolute top-1 right-3 z-20">
            <button
              onClick={() => setShowSource(false)}
              className="flex items-center gap-1 text-2xs px-2 py-1 rounded bg-brand-500/15 text-brand-400 hover:bg-brand-500/25 transition-colors"
              title="Switch to block view"
            >
              <Code2 size={12} />
              Block view
            </button>
          </div>
        </div>
        <SourceEditor onClose={() => setShowSource(false)} />
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full">
      <div className="relative">
        <MainPaneToolbar />
        {document && (
          <div className="absolute top-1 right-3 z-20">
            <button
              onClick={() => setShowSource(true)}
              className="flex items-center gap-1 text-2xs px-2 py-1 rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors"
              title="Edit raw source"
            >
              <Code2 size={12} />
              Source
            </button>
          </div>
        )}
      </div>
      <ParseErrorsBanner />
      <div ref={editor.containerRef} className="flex-1 flex overflow-hidden">
        {groups.map((group, gi) => (
          <Fragment key={`group-${gi}`}>
            {gi > 0 && (
              <PaneDivider
                onDrag={delta => editor.handleColumnDrag(gi - 1, delta)}
                onResizeEnd={() => {}}
                onDoubleClick={editor.handleResetDivider}
              />
            )}
            <div
              className={`flex flex-col overflow-hidden ${gi === focusedGroupIndex ? '' : 'opacity-90'}`}
              style={{flex: `${widths[gi]} 0 0`, minWidth: 200}}
              onMouseDown={() => {
                // Focus follows ANY click inside the group, content included.
                // selectBlock/selectSubflow route openInGroup to the focused
                // group, so if clicking a block in an unfocused pane didn't
                // move focus first, it would retarget the OTHER pane's tabs.
                if (gi !== focusedGroupIndex) editor.focusGroup(gi)
              }}
              onDragOver={e => {
                e.preventDefault()
                e.dataTransfer.dropEffect = 'move'
              }}
              onDrop={e => {
                e.preventDefault()
                const data = e.dataTransfer.getData('application/tab-move')
                if (!data) return
                try {
                  const {fromGroup, subflowId} = JSON.parse(data)
                  if (fromGroup !== gi) editor.moveTabToGroup(fromGroup, subflowId, gi)
                } catch {
                  /* ignore invalid drop data */
                }
              }}
            >
              <GroupTabStrip
                document={document}
                group={group}
                groupIndex={gi}
                isFocused={gi === focusedGroupIndex}
                totalGroups={groups.length}
                onSelectTab={subflowId => editor.openInGroup(subflowId, gi)}
                onCloseTab={subflowId => editor.closeTab(gi, subflowId)}
                onCloseAllTabs={() => editor.closeAllTabs(gi)}
                onCloseOtherTabs={subflowId => editor.closeOtherTabs(gi, subflowId)}
                onCloseGroup={() => editor.closeGroup(gi)}
              />
              {group.activeTabId && gi === focusedGroupIndex && <Breadcrumbs />}
              <div className="flex-1 flex flex-col overflow-hidden">
                {mainPaneView === 'diff'
                  ? gi === 0 && (
                      <ErrorBoundary>
                        <Suspense fallback={<Spinner />}>
                          <RegressionDiffView key="global-diff" />
                        </Suspense>
                      </ErrorBoundary>
                    )
                  : group.activeTabId &&
                    (mainPaneView === 'block' ? (
                      <div key={group.activeTabId} className="flex-1 overflow-y-auto">
                        <BlockView subflowId={group.activeTabId} />
                      </div>
                    ) : mainPaneView === 'graph' ? (
                      <ErrorBoundary>
                        <Suspense fallback={<Spinner />}>
                          <GraphView key={group.activeTabId} subflowId={group.activeTabId} />
                        </Suspense>
                      </ErrorBoundary>
                    ) : mainPaneView === 'local-map' ? (
                      <ErrorBoundary>
                        <Suspense fallback={<Spinner />}>
                          <ExecutionGraphView key={`local-map-${group.activeTabId}`} subflowId={group.activeTabId} />
                        </Suspense>
                      </ErrorBoundary>
                    ) : mainPaneView === 'map' ? (
                      <ErrorBoundary>
                        <Suspense fallback={<Spinner />}>
                          <ExecutionGraphView key="global-map" />
                        </Suspense>
                      </ErrorBoundary>
                    ) : null)}
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
  const [menuPos, setMenuPos] = useState<{x: number; y: number; tabId: string} | null>(null)
  const subflowMap = useMemo(() => {
    const m = new Map<string, (typeof document.subflows)[0]>()
    for (const sf of document.subflows) m.set(sf.id, sf)
    return m
  }, [document])
  if (group.tabs.length === 0) return null

  return (
    <div
      className={`group-header flex items-center border-b flex-shrink-0 overflow-hidden ${
        isFocused ? 'border-brand-500/30 bg-surface-0' : 'border-border-default bg-surface-1'
      }`}
    >
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
              onClick: () => onCloseTab(tabId),
            },
            {
              label: 'Close Others',
              icon: MinusSquare,
              onClick: () => onCloseOtherTabs(tabId),
            },
            {
              label: 'Close All',
              icon: XCircle,
              onClick: () => onCloseAllTabs(),
            },
          ]

          return (
            <Fragment key={tabId}>
              <button
                draggable
                onDragStart={e => {
                  e.dataTransfer.setData(
                    'application/tab-move',
                    JSON.stringify({fromGroup: groupIndex, subflowId: tabId}),
                  )
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
                // Activate on mousedown, not click: WebKitGTK (the Tauri
                // Linux webview) starts an HTML5 drag on draggable elements
                // after ~1px of movement and swallows the ensuing click, so
                // click-based switching randomly "does nothing" there.
                onMouseDown={e => {
                  if (e.button === 0) onSelectTab(tabId)
                }}
              >
                <span className="truncate max-w-[120px]">
                  {sf.sourceFile ? sf.sourceFile.replace(/\.txt$/i, '') : sf.name}
                </span>
                <span
                  className="inline-flex items-center justify-center w-4 h-4 rounded hover:bg-surface-3 opacity-0 group-hover/tab:opacity-100 transition-opacity flex-shrink-0"
                  onMouseDown={e => e.stopPropagation()}
                  onClick={e => {
                    e.stopPropagation()
                    onCloseTab(tabId)
                  }}
                >
                  <X size={11} />
                </span>
              </button>
              {menuPos && menuPos.tabId === tabId && (
                <ContextMenu x={menuPos.x} y={menuPos.y} onClose={() => setMenuPos(null)} items={contextItems} />
              )}
            </Fragment>
          )
        })}
      </div>
      {totalGroups > 1 && (
        <button
          className="flex items-center justify-center w-6 h-8 text-text-tertiary hover:text-text-secondary hover:bg-surface-2 transition-colors flex-shrink-0"
          onClick={e => {
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
