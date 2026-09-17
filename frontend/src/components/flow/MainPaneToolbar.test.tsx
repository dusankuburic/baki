import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import MainPaneToolbar from './MainPaneToolbar'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'
import {useEditorStore} from '@/stores/editorStore'
import {ToastProvider} from '@/components/shared/Toast'

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
      <MainPaneToolbar />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useUIStore.setState({mainPaneView: 'block'})
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
})
