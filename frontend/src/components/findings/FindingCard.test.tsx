import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {Finding, FlowDocument} from '@/types'

const previewFix = vi.fn()
const applyFix = vi.fn()
const suppressInSource = vi.fn()
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
    suppressInSource: (...a: unknown[]) => suppressInSource(...a),
  },
}))

vi.mock('@/components/shared/Portal', () => ({
  default: ({children}: {children: React.ReactNode}) => <>{children}</>,
}))

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
      <ConfirmProvider>
        <FindingCard finding={finding} blockLookup={new Map()} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: mockDoc, readOnly: false})
  useAnalysisStore.setState({
    reports: new Map(),
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
        <ConfirmProvider>
          <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    expect(screen.queryByText('Apply fix')).not.toBeInTheDocument()
    expect(screen.queryByText('Suppress in file')).not.toBeInTheDocument()
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

    const fixBtn = screen.getByText('Apply fix')
    fireEvent.click(fixBtn)

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

    const buttons = await screen.findAllByText('Apply fix')
    fireEvent.click(buttons[buttons.length - 1]!)

    await waitFor(() => {
      expect(applyFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'wrap-error-handler',
        'unhandled-error',
        undefined,
        undefined,
        true,
      )
    })

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

    await waitFor(() => {
      expect(applyFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'wrap-error-handler',
        'unhandled-error',
        undefined,
        undefined,
        false,
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

describe('FindingCard apply-fix gating (R0-2)', () => {
  beforeEach(() => {
    tauriMode = false
  })

  it('shows Apply fix for a cloud single-file flow; Suppress-in-file stays desktop-only', () => {
    useFlowStore.setState({document: {...mockDoc, filePath: '', isFolder: false} as unknown as FlowDocument})
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
        </ConfirmProvider>
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
        <ConfirmProvider>
          <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    expect(screen.getByText('Apply fix')).toBeInTheDocument()
  })

  it('desktop keeps both Apply fix and Suppress in file', () => {
    tauriMode = true
    useFlowStore.setState({document: mockDoc, readOnly: false})
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingCard finding={baseFinding} blockLookup={new Map()} onFixWithAI={vi.fn()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    expect(screen.getByText('Apply fix')).toBeInTheDocument()
    expect(screen.getByTitle(/Write a pad-ignore directive/)).toBeInTheDocument()
  })
})

describe('FindingCard cloud folder import (no stored source)', () => {
  afterEach(() => {
    tauriMode = true
  })

  it('disables Apply fix with the reason on a cloud folder import', () => {
    tauriMode = false
    useFlowStore.setState({
      document: {
        ...mockDoc,
        isFolder: true,
        files: [
          {path: 'Main.txt', name: 'Main.txt', size: 1},
          {path: 'Sub.txt', name: 'Sub.txt', size: 1},
        ],
      } as unknown as FlowDocument,
      readOnly: false,
    })
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingCard finding={baseFinding} blockLookup={new Map()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    const btn = screen.getByText('Apply fix').closest('button')
    expect(btn).toBeDisabled()
    expect(btn?.title).toMatch(/folder import/i)
  })

  it('keeps Apply fix enabled for a cloud single-file flow', () => {
    tauriMode = false
    renderCard()
    const btn = screen.getByText('Apply fix').closest('button')
    expect(btn).toBeEnabled()
    expect(btn?.title).not.toMatch(/folder import/i)
  })
})

describe('FindingCard mask-sensitive-variable', () => {
  it('passes metadata.variable to the preview', async () => {
    tauriMode = true
    previewFix.mockResolvedValue({original: 'old', patched: 'new'})
    renderCard({
      ...baseFinding,
      ruleId: 'sensitive-exposure',
      autoFix: 'mask-sensitive-variable',
      metadata: {variable: 'Password'},
    })

    fireEvent.click(screen.getByText('Apply fix'))

    await waitFor(() => {
      expect(previewFix).toHaveBeenCalledWith(
        'flow-1',
        'b1',
        'mask-sensitive-variable',
        'sensitive-exposure',
        'Password',
        undefined,
      )
    })
  })
})

describe('FindingCard suppress reason', () => {
  it('does not suppress when the reason prompt is cancelled', async () => {
    renderCard()
    fireEvent.click(screen.getByRole('button', {name: 'Suppress this finding'}))
    fireEvent.click(await screen.findByText('Cancel'))
    await waitFor(() => expect(screen.queryByText('Why is this being suppressed?')).not.toBeInTheDocument())
    expect(useAnalysisStore.getState().suppressedFindings).toHaveLength(0)
  })

  it('stores the entered reason', async () => {
    tauriMode = true
    renderCard()
    fireEvent.click(screen.getByRole('button', {name: 'Suppress this finding'}))
    const input = await screen.findByRole('textbox')
    fireEvent.change(input, {target: {value: 'Documented test endpoint'}})
    fireEvent.click(screen.getByRole('button', {name: 'Suppress'}))

    await waitFor(() => {
      expect(useAnalysisStore.getState().suppressedFindings.map(f => f.reason)).toEqual(['Documented test endpoint'])
    })
  })
})

describe('FindingCard suppress in file', () => {
  it('writes the directive via flowApi.suppressInSource', async () => {
    tauriMode = true
    suppressInSource.mockResolvedValue(mockDoc)
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: [], generatedAt: '2024', durationMs: 1, stats: {}})
    renderCard()

    fireEvent.click(screen.getByText('Suppress in file'))

    await waitFor(() => expect(suppressInSource).toHaveBeenCalledWith('flow-1', 'b1', 'unhandled-error'))
    await waitFor(() => expect(analyzeFlow).toHaveBeenCalled())
    expect(previewFix).not.toHaveBeenCalled()
  })
})
