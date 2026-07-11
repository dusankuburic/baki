import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import MetricsTab from './MetricsTab'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {AnalysisReport, FlowDocument, FlowMetrics} from '@/types'

vi.mock('@/api', () => ({
  analysisApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    getDataFlow: vi.fn().mockResolvedValue({taintPaths: [], deadData: []}),
  },
}))

vi.mock('recharts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => <div style={{width: 400, height: 200}}>{children}</div>,
  }
})

const doc = {
  id: 'flow1', name: 'Flow', filePath: '/f.txt', subflows: [],
  metadata: {blockCount: 0, subflowCount: 0, maxDepth: 0, parsedAt: '', fileSize: 0, rawLineCount: 0},
} as FlowDocument

function makeMetrics(over: Partial<FlowMetrics> = {}): FlowMetrics {
  return {
    subflows: [], totalBlocks: 10, totalVariables: 3, maxCyclomatic: 5, avgCyclomatic: 2.5,
    maxCognitive: 8, avgCognitive: 4, healthScore: 82, variableDensity: 0.3, subflowCount: 1,
    ...over,
  }
}

function makeReport(over: Partial<AnalysisReport> = {}): AnalysisReport {
  return {
    flowId: 'flow1', generatedAt: 't', findings: [],
    stats: {errors: 0, warnings: 0, info: 0, blocksAnalyzed: 0, rulesRun: 0}, durationMs: 0,
    ...over,
  }
}

const initialFlowState = useFlowStore.getState()
const initialAnalysisState = useAnalysisStore.getState()

describe('MetricsTab', () => {
  beforeEach(() => {
    useFlowStore.setState(initialFlowState, true)
    useAnalysisStore.setState(initialAnalysisState, true)
  })

  it('shows a placeholder when no flow is loaded', () => {
    render(<MetricsTab />)
    expect(screen.getByText('Load a flow to view metrics')).toBeTruthy()
  })

  it('shows a placeholder when a flow is loaded but not yet analyzed', () => {
    useFlowStore.setState({document: doc})
    render(<MetricsTab />)
    expect(screen.getByText('Run analysis to see metrics')).toBeTruthy()
  })

  it('renders the health score and stat cards once metrics are available', () => {
    useFlowStore.setState({document: doc})
    useAnalysisStore.setState({reports: new Map([['flow1', makeReport({metrics: makeMetrics({healthScore: 91})})]])})
    render(<MetricsTab />)
    expect(screen.getByText('91')).toBeTruthy()
    expect(screen.getByText('Total Blocks')).toBeTruthy()
  })

  // Regression test for the removed non-null assertion
  // (metrics.circularDependencies!.join(...)): the circular-dependencies
  // banner must render correctly from a properly-narrowed check, and must not
  // render at all when there are none.
  it('shows the circular dependencies banner only when present', () => {
    useFlowStore.setState({document: doc})
    useAnalysisStore.setState({reports: new Map([['flow1', makeReport({
      metrics: makeMetrics({circularDependencies: ['Main', 'Sub1', 'Main']}),
    })]])})
    render(<MetricsTab />)
    expect(screen.getByText('Circular Dependencies')).toBeTruthy()
    expect(screen.getByText(/Main → Sub1 → Main/)).toBeTruthy()
  })

  it('omits the circular dependencies banner when the list is empty', () => {
    useFlowStore.setState({document: doc})
    useAnalysisStore.setState({reports: new Map([['flow1', makeReport({
      metrics: makeMetrics({circularDependencies: []}),
    })]])})
    render(<MetricsTab />)
    expect(screen.queryByText('Circular Dependencies')).toBeNull()
  })

  it('omits the circular dependencies banner when undefined', () => {
    useFlowStore.setState({document: doc})
    useAnalysisStore.setState({reports: new Map([['flow1', makeReport({metrics: makeMetrics()})]])})
    render(<MetricsTab />)
    expect(screen.queryByText('Circular Dependencies')).toBeNull()
  })
})
