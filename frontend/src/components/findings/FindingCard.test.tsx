import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {Finding, FlowDocument} from '@/types'

const previewFix = vi.fn()
const applyFix = vi.fn()
const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
    getRelatedFindings: vi.fn().mockResolvedValue([]),
    exportHTML: vi.fn().mockResolvedValue(''),
  },
  flowApi: {
    previewFix: (...a: unknown[]) => previewFix(...a),
    applyFix: (...a: unknown[]) => applyFix(...a),
  },
}))

// Stub Portal so Modal renders in the test DOM tree (jsdom doesn't have
// createPortal's document.body in the same way).
vi.mock('@/components/shared/Portal', () => ({
  default: ({children}: {children: React.ReactNode}) => <>{children}</>,
}))

// Mock platform guards with a mutable flag — FindingCard guards Apply fix
// (desktop OR cloud single-file) and Suppress in file (desktop only) behind
// isTauri(); tests flip the flag to drive both modes.
let tauriMode = true
vi.mock('@/platform/guards', () => ({
  isTauri: () => tauriMode,
}))

import FindingCard from './FindingCard'

const mockDoc = {
  id: 'flow-1',
  name: 'Test',
  filePath: '/test.txt',
  subflows: [{id: 'sf1', name: 'Main', blocks: []}],
  metadata: {fileSize: 1000, totalBlocks: 10, totalSubflows: 1},
} as unknown as FlowDocument

const baseFinding: Finding = {
  id: 'f1',
  ruleId: 'unhandled-error',
  severity: 'warning',
  title: 'Unhandled error',
  description: 'No handler',
  blockId: 'b1',
  subflowId: 'sf1',
  category: 'Reliability',
  autoFix: 'wrap-error-handler',
  autoFixHint: 'Wrap in Try/Catch',
}

function renderCard(finding: Finding = baseFinding) {
  useFlowStore.setState({document: mockDoc, readOnly: false})
  return render(
    <ToastProvider>
      <FindingCard finding={finding} blockLookup={new Map()} />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: mockDoc, readOnly: false})
  useAnalysisStore.setState({
    reports: new Map(),
    findingsByBlock: new Map(),
    suppressedFindings: [],
    suppressedKeys: new Set(),
    triageMap: new Map(),
    isAnalyzing: false,
    analyzingGen: 0,
  })
})

describe('FindingCard read-only gating (U1.2)', () => {
  it('hides Apply fix and Suppress in file on read-only flows', () => {
    tauriMode = true
    useFlowStore.setState({readOnly: true})
    render(
      <ToastProvider>
        <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
      </ToastProvider>,
    )
    expect(screen.queryByText('Apply fix')).not.toBeInTheDocument()
    expect(screen.queryByText('Suppress in file')).not.toBeInTheDocument()
    // Fix with AI is a suggestion, not a source write — stays available.
    expect(screen.getByText('Fix with AI')).toBeInTheDocument()
  })
})

describe('FindingCard apply-fix chain', () => {
  it('opens preview modal, then applies the fix and re-analyzes', async () => {
    previewFix.mockResolvedValue({original: 'old code', patched: 'new code'})
    const updatedDoc = {...mockDoc, name: 'Updated'}
    applyFix.mockResolvedValue(updatedDoc)
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: [], generatedAt: '2024', durationMs: 1, stats: {}})

    renderCard()

    // Click "Apply fix" button (the one tied to finding.autoFix, not the suppress one).
    const fixBtn = screen.getByText('Apply fix')
    fireEvent.click(fixBtn)

    // Preview modal should open with the diff.
    await waitFor(() => {
      expect(previewFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'wrap-error-handler',
        'unhandled-error',
        undefined,
        undefined,
      )
    })

    // Click "Apply fix" in the modal to confirm. There are now two "Apply fix"
    // buttons (the card's + the modal's) — click the last one (the modal's).
    const buttons = await screen.findAllByText('Apply fix')
    fireEvent.click(buttons[buttons.length - 1])

    await waitFor(() => {
      expect(applyFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'wrap-error-handler',
        'unhandled-error',
        undefined,
        undefined,
      )
    })

    // The updated document is set and re-analysis runs.
    await waitFor(() => {
      expect(analyzeFlow).toHaveBeenCalled()
    })
    expect(useFlowStore.getState().document).toEqual(updatedDoc)
  })

  it('passes metadata.variable for init-variable fixType', async () => {
    previewFix.mockResolvedValue({original: 'old', patched: 'new'})
    applyFix.mockResolvedValue(mockDoc)
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: [], generatedAt: '2024', durationMs: 1, stats: {}})

    const finding: Finding = {
      ...baseFinding,
      autoFix: 'init-variable',
      metadata: {variable: 'MyVar'},
    }
    renderCard(finding)

    fireEvent.click(screen.getByText('Apply fix'))

    await waitFor(() => {
      expect(previewFix).toHaveBeenCalledWith('flow-1', 'b1', 'init-variable', 'unhandled-error', 'MyVar', undefined)
    })
  })

  it('passes metadata.property for replace-with-variable fixType', async () => {
    previewFix.mockResolvedValue({original: 'old', patched: 'new'})
    applyFix.mockResolvedValue(mockDoc)
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: [], generatedAt: '2024', durationMs: 1, stats: {}})

    const finding: Finding = {
      ...baseFinding,
      autoFix: 'replace-with-variable',
      metadata: {property: 'ApiKey'},
    }
    renderCard(finding)

    fireEvent.click(screen.getByText('Apply fix'))

    await waitFor(() => {
      expect(previewFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'replace-with-variable',
        'unhandled-error',
        undefined,
        'ApiKey',
      )
    })
  })

  it('falls back to direct apply when preview fails', async () => {
    previewFix.mockRejectedValue(new Error('preview blocked'))
    applyFix.mockResolvedValue(mockDoc)
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: [], generatedAt: '2024', durationMs: 1, stats: {}})

    renderCard()

    fireEvent.click(screen.getByText('Apply fix'))

    // Preview failed → should call applyFix directly without opening the modal.
    await waitFor(() => {
      expect(applyFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'wrap-error-handler',
        'unhandled-error',
        undefined,
        undefined,
      )
    })
    expect(analyzeFlow).toHaveBeenCalled()
  })

  it('shows error toast when applyFix fails', async () => {
    previewFix.mockRejectedValue(new Error('preview blocked'))
    applyFix.mockRejectedValue(new Error('permission denied'))

    renderCard()

    fireEvent.click(screen.getByText('Apply fix'))

    await waitFor(() => {
      expect(screen.getByText('Could not apply fix')).toBeInTheDocument()
    })
  })
})

// R0-2: single-finding Apply fix is available in cloud mode too (single-file
// flows) — it was desktop-gated while the bulk bar's applyFixBatch already
// worked in cloud, a pure inconsistency. Suppress-in-file stays desktop-only
// (it writes the raw file path).
describe('FindingCard apply-fix gating (R0-2)', () => {
  beforeEach(() => {
    tauriMode = false
  })

  it('shows Apply fix for a cloud single-file flow; Suppress-in-file stays desktop-only', () => {
    useFlowStore.setState({document: {...mockDoc, filePath: '', isFolder: false} as unknown as FlowDocument})
    render(
      <ToastProvider>
        <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
      </ToastProvider>,
    )
    expect(screen.getByText('Apply fix')).toBeInTheDocument()
    expect(screen.getByTitle(/stored flow source/)).toBeInTheDocument()
    expect(screen.queryByTitle(/Write a pad-ignore directive/)).not.toBeInTheDocument()
  })

  it('shows Apply fix for a cloud multi-file (folder) flow too (R3-3)', () => {
    useFlowStore.setState({document: {...mockDoc, filePath: '', isFolder: true} as unknown as FlowDocument})
    render(
      <ToastProvider>
        <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
      </ToastProvider>,
    )
    expect(screen.getByText('Apply fix')).toBeInTheDocument()
  })

  it('desktop keeps both Apply fix and Suppress in file', () => {
    tauriMode = true
    useFlowStore.setState({document: mockDoc, readOnly: false})
    render(
      <ToastProvider>
        <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
      </ToastProvider>,
    )
    expect(screen.getByText('Apply fix')).toBeInTheDocument()
    expect(screen.getByTitle(/Write a pad-ignore directive/)).toBeInTheDocument()
  })
})
