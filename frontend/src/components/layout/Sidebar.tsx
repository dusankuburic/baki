import {useTranslation} from 'react-i18next'
import {useCallback, useMemo} from 'react'
import {useKeyboard} from '@/hooks/useKeyboard'
import {FolderOpen, FolderTree, BarChart2, Library} from 'lucide-react'
import Button from '@/components/shared/Button'
import FileHeader from '@/components/sidebar/FileHeader'
import FileList from '@/components/sidebar/FileList'
import SearchBar from '@/components/sidebar/SearchBar'
import FilterChips from '@/components/sidebar/FilterChips'
import FlowTree from '@/components/sidebar/FlowTree'
import VariablesTab from '@/components/sidebar/VariablesTab'
import LibraryTab from '@/components/sidebar/LibraryTab'
import SidebarToolbar from '@/components/sidebar/SidebarToolbar'
import {useFlowStore, ALL_TYPES} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore, isSystemView} from '@/stores/uiStore'
import {useToast} from '@/components/shared/Toast'
import {Tabs} from '@/components/shared'
import {useFileOpen} from '@/components/sidebar/hooks/useFileOpen'
import {useSidebarSearch} from '@/components/sidebar/hooks/useSidebarSearch'
import {analysisApi} from '@/api'
import type {BlockType, AnalysisReport, Block} from '@/types'

export default function Sidebar() {
  const {t} = useTranslation('shell')
  const document = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const visibleBlockId = useFlowStore(s => s.visibleBlockId)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
  const expandedSubflowIds = useFlowStore(s => s.expandedSubflowIds)
  const expandedBlockIds = useFlowStore(s => s.expandedBlockIds)
  const visibleTypes = useFlowStore(s => s.visibleTypes)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const toggleSubflowExpand = useFlowStore(s => s.toggleSubflowExpand)
  const toggleBlockExpand = useFlowStore(s => s.toggleBlockExpand)
  const setVisibleTypes = useFlowStore(s => s.setVisibleTypes)
  const folderFiles = useFlowStore(s => s.folderFiles)
  const selectedFilePath = useFlowStore(s => s.selectedFilePath)

  const sidebarTab = useUIStore(s => s.sidebarTab)
  const setSidebarTab = useUIStore(s => s.setSidebarTab)
  const setInspectorTab = useUIStore(s => s.setInspectorTab)

  const {
    recentFiles,
    isLoading,
    handleNewFlow,
    handleOpenFile,
    handleOpenFolder,
    handleSelectFolderFile,
    handleLoadRecent,
    handleRemoveRecent,
    handleClearRecent,
    handleRevealFile,
    handleReloadFile,
  } = useFileOpen()

  const {
    searchQuery,
    handleQueryChange,
    searchResults,
    matchedBlockIds,
    searchHighlightsMap,
    activeResultIndex,
    nextResult,
    prevResult,
  } = useSidebarSearch()

  const handleSelectBlock = useCallback(
    (blockId: string, subflowId: string) => {
      selectBlock(blockId)
      selectSubflow(subflowId)
      if (isSystemView(useUIStore.getState().mainPaneView)) {
        useUIStore.getState().setMainPaneView('block')
      }
    },
    [selectBlock, selectSubflow],
  )

  const handleToggleType = useCallback(
    (type: BlockType) => {
      const next = new Set(visibleTypes)
      if (next.has(type)) {
        next.delete(type)
        if (next.size === 0) {
          setVisibleTypes(new Set(ALL_TYPES))
          return
        }
      } else {
        next.add(type)
      }
      setVisibleTypes(next)
    },
    [visibleTypes, setVisibleTypes],
  )

  const handleSelectAll = useCallback(() => setVisibleTypes(new Set(ALL_TYPES)), [setVisibleTypes])

  useKeyboard({
    scope: 'sidebar',
    handlers: {
      'tree.expand': () => {
        if (selectedSubflowId && !expandedSubflowIds.has(selectedSubflowId)) {
          toggleSubflowExpand(selectedSubflowId)
        }
      },
      'tree.collapse': () => {
        if (selectedSubflowId && expandedSubflowIds.has(selectedSubflowId)) {
          toggleSubflowExpand(selectedSubflowId)
        }
      },
      'tree.expand.all': () => {
        if (!document) return
        for (const sf of document.subflows) {
          if (!expandedSubflowIds.has(sf.id)) toggleSubflowExpand(sf.id)
        }
      },
      'tree.collapse.all': () => {
        if (!document) return
        for (const sf of document.subflows) {
          if (expandedSubflowIds.has(sf.id)) toggleSubflowExpand(sf.id)
        }
      },
    },
  })

  const toast = useToast()
  const setReport = useAnalysisStore(s => s.setReport)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const setAnalysisProgress = useAnalysisStore(s => s.setProgress)

  const analysisReports = useAnalysisStore(s => s.reports)
  const findingCounts = useMemo(() => {
    if (!document) return undefined
    const report = analysisReports.get(document.id)
    if (!report) return undefined
    const counts = new Map<string, number>()
    for (const f of report.findings) {
      counts.set(f.blockId, (counts.get(f.blockId) ?? 0) + 1)
    }
    return counts
  }, [document, analysisReports])

  const handleAnalyze = useCallback(async () => {
    if (!document) return
    if (useAnalysisStore.getState().isAnalyzing) return
    setInspectorTab('findings')
    const gen = beginAnalyzing()
    setAnalysisProgress({current: 0, total: 0, ruleName: ''})
    try {
      const r = await analysisApi.analyzeFlow()
      if (r) setReport(document.id, r as AnalysisReport)
    } catch (err) {
      toast.error('Analysis failed: ' + (err as Error).message)
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) {
        setAnalyzing(false)
      }
    }
  }, [document, setReport, setAnalyzing, beginAnalyzing, setAnalysisProgress, setInspectorTab, toast])

  const filterChips = useMemo(() => {
    if (!document) return []
    const counts = new Map<string, number>()
    for (const sf of document.subflows) countTypes(sf.blocks, counts)
    const chips = ALL_TYPES.map(type => ({
      type,
      // Falls back to the raw enum for a type with no label key yet.
      label: t(`blockTypes.${type}`, {defaultValue: type}),
      count: counts.get(type) ?? 0,
    }))
    return [{type: 'ALL' as const, label: t('blockTypes.ALL'), count: 0}, ...chips]
  }, [document, t])

  const folderName = useMemo(() => {
    if (!folderFiles?.length) return ''
    const p = folderFiles[0].path
    const parts = p.split(p.includes('\\') ? '\\' : '/')
    return parts.length > 1 ? parts[parts.length - 2] : ''
  }, [folderFiles])

  return (
    <div className="flex flex-col h-full bg-surface-1">
      <FileHeader
        document={document}
        recentFiles={recentFiles}
        isLoading={isLoading}
        onNewFlow={handleNewFlow}
        onOpenFile={handleOpenFile}
        onOpenFolder={handleOpenFolder}
        onLoadRecent={handleLoadRecent}
        onRemoveRecent={handleRemoveRecent}
        onClearRecent={handleClearRecent}
      />

      {folderFiles && folderFiles.length > 0 && (
        <FileList
          files={folderFiles}
          selectedFilePath={selectedFilePath}
          folderName={folderName}
          onSelectFile={handleSelectFolderFile}
          onRevealFile={handleRevealFile}
          onReloadFile={handleReloadFile}
        />
      )}

      <Tabs
        items={[
          {value: 'explorer' as const, label: t('sidebar.explorer'), icon: FolderTree},
          {value: 'variables' as const, label: t('sidebar.variables'), icon: BarChart2},
          {value: 'library' as const, label: t('sidebar.library'), icon: Library},
        ]}
        value={sidebarTab}
        onChange={setSidebarTab}
        aria-label={t('sidebar.sectionsAria')}
        panelIdPrefix="sidebar-panel"
        className="h-10 px-2 border-b border-border-default bg-surface-1"
      />

      <div
        id={`sidebar-panel-${sidebarTab}`}
        role="tabpanel"
        aria-label={t('sidebar.panelAria')}
        className="flex-1 min-h-0 flex flex-col"
      >
        {sidebarTab === 'explorer' ? (
          <>
            <SearchBar
              value={searchQuery}
              onChange={handleQueryChange}
              disabled={!document}
              resultCount={searchResults.length}
              activeIndex={activeResultIndex}
              onNextResult={nextResult}
              onPrevResult={prevResult}
            />

            {document && (
              <FilterChips
                chips={filterChips}
                activeTypes={visibleTypes}
                onToggle={handleToggleType}
                onSelectAll={handleSelectAll}
              />
            )}

            {document ? (
              <FlowTree
                document={document}
                selectedBlockId={selectedBlockId}
                visibleBlockId={visibleBlockId}
                selectedSubflowId={selectedSubflowId}
                expandedSubflowIds={expandedSubflowIds}
                expandedBlockIds={expandedBlockIds}
                visibleTypes={visibleTypes}
                searchQuery={searchQuery}
                matchedBlockIds={matchedBlockIds}
                searchHighlights={searchHighlightsMap}
                findingCounts={findingCounts}
                onSelectBlock={handleSelectBlock}
                onSelectSubflow={id => {
                  selectSubflow(id)
                  if (isSystemView(useUIStore.getState().mainPaneView)) {
                    useUIStore.getState().setMainPaneView('block')
                  }
                }}
                onToggleSubflowExpand={toggleSubflowExpand}
                onToggleBlockExpand={toggleBlockExpand}
              />
            ) : (
              <div className="flex-1 flex flex-col items-center justify-center px-6 text-center">
                <div className="w-16 h-16 rounded-full bg-surface-2 flex items-center justify-center mb-4">
                  <FolderOpen size={28} className="text-text-tertiary" />
                </div>
                <h3 className="text-sm font-semibold text-text-primary mb-1">{t('sidebar.noFlowTitle')}</h3>
                <p className="text-xs text-text-tertiary mb-4 leading-relaxed">{t('sidebar.noFlowBody')}</p>
                <div className="flex gap-2">
                  <Button size="sm" variant="primary" onClick={handleOpenFile}>
                    {t('sidebar.openFile')}
                  </Button>
                  <Button size="sm" variant="secondary" onClick={handleOpenFolder}>
                    {t('sidebar.openFolder')}
                  </Button>
                </div>
              </div>
            )}
          </>
        ) : sidebarTab === 'variables' ? (
          <VariablesTab />
        ) : (
          <LibraryTab />
        )}
      </div>

      <SidebarToolbar hasFlow={!!document} isAnalyzing={isAnalyzing} onAnalyze={handleAnalyze} />
    </div>
  )
}

function countTypes(blocks: Block[], counts: Map<string, number>) {
  for (const block of blocks) {
    counts.set(block.type, (counts.get(block.type) ?? 0) + 1)
    if (block.children?.length) countTypes(block.children, counts)
  }
}
