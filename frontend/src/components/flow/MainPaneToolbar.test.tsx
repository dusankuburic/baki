import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
import MainPaneToolbar from './MainPaneToolbar'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'
import {useEditorStore} from '@/stores/editorStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'

const exportPDF = vi.fn()
const exportHTML = vi.fn()
const exportMarkdown = vi.fn()
const pickFile = vi.fn()
const compareCurrentWith = vi.fn()
const reimport = vi.fn()
const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  exportApi: {
    exportPDF: (...a: unknown[]) => exportPDF(...a),
    exportHTML: (...a: unknown[]) => exportHTML(...a),
    exportMarkdown: (...a: unknown[]) => exportMarkdown(...a),
    pickFile: (...a: unknown[]) => pickFile(...a),
    compareCurrentWith: (...a: unknown[]) => compareCurrentWith(...a),
  },
  flowApi: {reimport: (...a: unknown[]) => reimport(...a)},
  analysisApi: {analyzeFlow: (...a: unknown[]) => analyzeFlow(...a)},
}))

// Re-import needs a filesystem (desktop) and a file-backed flow; the toolbar
// asks the platform guard, so the tests control it here.
const capabilities = {fileSystem: true}
vi.mock('@/platform/guards', async importOriginal => ({
  ...(await importOriginal<typeof import('@/platform/guards')>()),
  getPlatformCapabilities: () => capabilities,
}))

function renderToolbar() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <MainPaneToolbar />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

// jsdom has no Fullscreen API. installFullscreenApi adds one whose
// request/exit calls flip `fullscreenElement` and fire `fullscreenchange`,
// like a browser; the returned function removes it again.
function installFullscreenApi() {
  let el: Element | null = null
  const request = vi.fn(() => {
    el = document.documentElement
    document.dispatchEvent(new Event('fullscreenchange'))
    return Promise.resolve()
  })
  const exit = vi.fn(() => {
    el = null
    document.dispatchEvent(new Event('fullscreenchange'))
    return Promise.resolve()
  })
  Object.defineProperty(document, 'fullscreenElement', {configurable: true, get: () => el})
  Object.defineProperty(document.documentElement, 'requestFullscreen', {configurable: true, value: request})
  Object.defineProperty(document, 'exitFullscreen', {configurable: true, value: exit})
  const uninstall = () => {
    delete (document as {fullscreenElement?: unknown}).fullscreenElement
    delete (document.documentElement as {requestFullscreen?: unknown}).requestFullscreen
    delete (document as {exitFullscreen?: unknown}).exitFullscreen
  }
  return {request, exit, leaveExternally: () => exit(), uninstall}
}

beforeEach(() => {
  vi.clearAllMocks()
  useUIStore.setState({mainPaneView: 'block', showSourceEditor: false, sourceEditorDirty: false})
  capabilities.fileSystem = true
  useFlowStore.setState({document: {id: 'flow-1', name: 'F', filePath: '/flows/F.txt'} as never})
  useAnalysisStore.setState({isAnalyzing: false, analyzingGen: 0})
  useEditorStore.setState({groups: [{tabs: [], activeTabId: null} as never], focusedGroupIndex: 0})
})

describe('MainPaneToolbar', () => {
  it('exports PDF and surfaces the chosen path', async () => {
    exportPDF.mockResolvedValue('/tmp/report.pdf')
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'Export PDF'}))

    await waitFor(() => expect(exportPDF).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(screen.getByText('Exported to /tmp/report.pdf')).toBeInTheDocument())
  })

  it('exports HTML via the html handler', async () => {
    exportHTML.mockResolvedValue('')
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'Export HTML'}))

    await waitFor(() => expect(exportHTML).toHaveBeenCalledTimes(1))
  })

  it('reports export failures as an error toast', async () => {
    exportPDF.mockRejectedValue(new Error('disk full'))
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'Export PDF'}))

    await waitFor(() => expect(screen.getByText(/Export failed: disk full/)).toBeInTheDocument())
  })

  it('compares against a picked old version and switches to the diff view', async () => {
    pickFile.mockResolvedValue('/tmp/old.txt')
    compareCurrentWith.mockResolvedValue({blocksAdded: 1})
    useUIStore.setState({mainPaneView: 'diff'})
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'New Comparison'}))

    await waitFor(() => expect(compareCurrentWith).toHaveBeenCalledWith('/tmp/old.txt'))
    await waitFor(() => expect(useUIStore.getState().mainPaneView).toBe('diff'))
    await waitFor(() => expect(screen.getByText('Comparison complete')).toBeInTheDocument())
  })

  it('aborts comparison when no file is picked', async () => {
    pickFile.mockResolvedValue(null)
    useUIStore.setState({mainPaneView: 'diff'})
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'New Comparison'}))

    // Early return happens BEFORE the "Comparing flows..." toast.
    await waitFor(() => expect(pickFile).toHaveBeenCalled())
    expect(compareCurrentWith).not.toHaveBeenCalled()
    expect(screen.queryByText('Comparing flows...')).not.toBeInTheDocument()
    expect(useUIStore.getState().mainPaneView).toBe('diff')
  })

  it('re-imports the flow, re-analyzes, and restores the document', async () => {
    const fresh = {id: 'flow-1', name: 'F2', subflows: [], metadata: {}} as never
    reimport.mockResolvedValue(fresh)
    analyzeFlow.mockResolvedValue({
      flowId: 'flow-1',
      findings: [],
      stats: {errors: 0, warnings: 0, info: 0},
      generatedAt: '2024-01-01T00:00:00Z',
      durationMs: 5,
    } as never)
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'Re-import flow'}))

    await waitFor(() => expect(reimport).toHaveBeenCalledWith('flow-1'))
    await waitFor(() => expect(analyzeFlow).toHaveBeenCalledTimes(1))
    // setDocument rebuilds indexes/derived state, so assert identity not shape.
    await waitFor(() => expect(useFlowStore.getState().document?.name).toBe('F2'))
    await waitFor(() => expect(screen.getAllByText(/Flow re-imported and re-analyzed/).length).toBeGreaterThan(0))
  })

  it('hides re-import where it cannot work (no filesystem, or an uploaded flow)', () => {
    capabilities.fileSystem = false
    const {unmount} = renderToolbar()
    expect(screen.queryByRole('button', {name: 'Re-import flow'})).not.toBeInTheDocument()
    unmount()

    capabilities.fileSystem = true
    useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
    renderToolbar()
    expect(screen.queryByRole('button', {name: 'Re-import flow'})).not.toBeInTheDocument()
  })

  it('disables re-import while one is in flight', async () => {
    let resolveReimport: (v: unknown) => void
    reimport.mockReturnValue(new Promise(r => (resolveReimport = r)))
    renderToolbar()

    const btn = screen.getByRole('button', {name: 'Re-import flow'})
    fireEvent.click(btn)
    const busy = screen.getByRole('button', {name: 'Re-importing…'})
    expect(busy).toBeDisabled()

    resolveReimport!({id: 'flow-1'})
    await waitFor(() => expect(screen.getByRole('button', {name: 'Re-import flow'})).toBeEnabled())
  })

  it('swallows a refused fullscreen request instead of leaking an unhandled rejection', async () => {
    const refusal = Promise.reject(new TypeError('Permissions check failed'))
    const caught = vi.spyOn(refusal, 'catch')
    const el = document.documentElement as HTMLElement & {requestFullscreen: () => Promise<void>}
    const original = el.requestFullscreen
    el.requestFullscreen = vi.fn(() => refusal)
    try {
      renderToolbar()
      fireEvent.click(screen.getByRole('button', {name: 'Fullscreen'}))
      expect(el.requestFullscreen).toHaveBeenCalledTimes(1)
      // The rejection is handled on the returned promise itself.
      expect(caught).toHaveBeenCalledTimes(1)
      await expect(refusal.catch(() => 'handled')).resolves.toBe('handled')
    } finally {
      el.requestFullscreen = original
    }
  })

  it('leaves fullscreen on the second click, and follows an exit it did not trigger', () => {
    const fs = installFullscreenApi()
    try {
      renderToolbar()
      fireEvent.click(screen.getByRole('button', {name: 'Fullscreen'}))
      expect(fs.request).toHaveBeenCalledTimes(1)
      expect(screen.queryByRole('button', {name: 'Fullscreen'})).not.toBeInTheDocument()

      // The second click used to request fullscreen again (a no-op), leaving
      // Escape as the only way back.
      fireEvent.click(screen.getByRole('button', {name: 'Exit fullscreen'}))
      expect(fs.exit).toHaveBeenCalledTimes(1)
      expect(fs.request).toHaveBeenCalledTimes(1)
      expect(screen.getByRole('button', {name: 'Fullscreen'})).toBeInTheDocument()

      // Escape / the browser's own controls fire the same event.
      fireEvent.click(screen.getByRole('button', {name: 'Fullscreen'}))
      expect(screen.getByRole('button', {name: 'Exit fullscreen'})).toBeInTheDocument()
      act(() => {
        void fs.leaveExternally()
      })
      expect(screen.getByRole('button', {name: 'Fullscreen'})).toBeInTheDocument()
    } finally {
      fs.uninstall()
    }
  })

  it('offers Source as a view: it opens the raw editor, and any other view closes it', async () => {
    renderToolbar()
    fireEvent.click(screen.getByRole('radio', {name: 'Source'}))
    expect(useUIStore.getState().showSourceEditor).toBe(true)
    expect(screen.getByRole('radio', {name: 'Source'})).toBeChecked()
    expect(screen.getByRole('radio', {name: 'Block'})).not.toBeChecked()

    fireEvent.click(screen.getByRole('radio', {name: 'Graph'}))
    await waitFor(() => expect(useUIStore.getState().showSourceEditor).toBe(false))
    expect(useUIStore.getState().mainPaneView).toBe('graph')
    expect(screen.getByRole('radio', {name: 'Graph'})).toBeChecked()
  })

  it('confirms before a view click discards unsaved source edits', async () => {
    useUIStore.setState({showSourceEditor: true, sourceEditorDirty: true})
    renderToolbar()

    fireEvent.click(screen.getByRole('radio', {name: 'Block'}))
    expect(await screen.findByText('Discard unsaved changes?')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(screen.queryByText('Discard unsaved changes?')).not.toBeInTheDocument())
    expect(useUIStore.getState().showSourceEditor).toBe(true)

    fireEvent.click(screen.getByRole('radio', {name: 'Block'}))
    fireEvent.click(await screen.findByRole('button', {name: 'Discard'}))
    await waitFor(() => expect(useUIStore.getState().showSourceEditor).toBe(false))
  })

  it('hides the Source view and the block tools where they do not apply', () => {
    useFlowStore.setState({document: null})
    const {unmount} = renderToolbar()
    expect(screen.queryByRole('radio', {name: 'Source'})).not.toBeInTheDocument()
    unmount()

    useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
    useUIStore.setState({showSourceEditor: true})
    renderToolbar()
    // The complexity map colours block cards; the source editor has none.
    expect(screen.queryByRole('button', {name: 'Complexity Map'})).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Fullscreen'})).toBeInTheDocument()
  })

  it('names every view, so the icon-only collapse keeps its labels', () => {
    renderToolbar()
    expect(screen.getByRole('radiogroup', {name: 'Flow view'})).toBeInTheDocument()
    for (const name of ['Block', 'Graph', 'Local Map', 'Map', 'Diff', 'Source']) {
      expect(screen.getByRole('radio', {name})).toHaveAttribute('aria-label', name)
    }
  })

  it('folds Fit and the PNG export into the More menu in the graph views, keeping zoom inline', () => {
    useUIStore.setState({mainPaneView: 'graph'})
    renderToolbar()
    expect(screen.getByRole('button', {name: 'Zoom in'})).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'More actions'}))
    expect(screen.getByRole('menuitem', {name: 'Fit to screen'})).toBeInTheDocument()
    expect(screen.getByRole('menuitem', {name: 'Export graph as PNG'})).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', {name: 'Zoom in'})).not.toBeInTheDocument()
  })

  it('folds re-import and the exports into a More menu for narrow panes', async () => {
    exportPDF.mockResolvedValue('')
    renderToolbar()

    fireEvent.click(screen.getByRole('button', {name: 'More actions'}))
    expect(screen.getByRole('menuitem', {name: 'Re-import flow'})).toBeInTheDocument()
    expect(screen.getByRole('menuitem', {name: 'Export HTML'})).toBeInTheDocument()
    fireEvent.click(screen.getByRole('menuitem', {name: 'Export PDF'}))

    await waitFor(() => expect(exportPDF).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })
})
