
import {describe, it, expect, vi} from 'vitest'
import {useAnalysisStore} from '@/stores/analysisStore'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

const fullReport = {
  flowId: 'flow-abc-123',
  flowName: 'Customer Sync Flow',
  generatedAt: '2024-07-12T10:30:00Z',
  durationMs: 42,
  findings: [
    {
      id: 'F-001',
      fingerprint: 'hardcoded-credential:b1:ApiKey',
      ruleId: 'hardcoded-credential',
      severity: 'error',
      confidence: 'high',
      title: 'Hardcoded API key',
      description: 'An AWS-shaped API key literal was found in a SetVariable action.',
      blockId: 'b1',
      subflowId: 'sf-main',
      suggestion: 'Replace the literal with a stored secret reference.',
      autoFixHint: 'Use a vault-backed variable instead of the literal.',
      autoFix: 'replace-with-variable',
      category: 'Security',
      metadata: {property: 'Value', variable: 'ApiKey'},
    },
    {
      id: 'F-002',
      fingerprint: 'unhandled-error:b2',
      ruleId: 'unhandled-error',
      severity: 'warning',
      confidence: 'medium',
      title: 'Unhandled error in HTTP action',
      description: 'HTTPClient.InvokeService has no error handler.',
      blockId: 'b2',
      subflowId: 'sf-main',
      suggestion: 'Wrap in a Try/Catch block.',
      autoFix: 'wrap-error-handler',
      category: 'Reliability',
      metadata: {},
    },
  ],
  stats: {
    errors: 1,
    warnings: 1,
    info: 0,
    blocksAnalyzed: 15,
    rulesRun: 29,
    suppressed: 0,
  },
  metrics: {
    healthScore: 73,
    totalBlocks: 15,
    totalSubflows: 2,
    maxDepth: 4,
    cyclomaticComplexity: 8,
  },
  groups: [
    {
      blockId: 'b1',
      primary: {
        id: 'F-001',
        ruleId: 'hardcoded-credential',
        severity: 'error',
        title: 'Hardcoded API key',
        description: '...',
        blockId: 'b1',
        subflowId: 'sf-main',
      },
      findings: [],
      duplicateCount: 0,
    },
  ],
  ruleProfiles: [
    {ruleId: 'hardcoded-credential', durationMs: 2, blocksChecked: 15, findingsEmitted: 1},
    {ruleId: 'unhandled-error', durationMs: 1, blocksChecked: 15, findingsEmitted: 1},
  ],
}

describe('AnalysisReport integration: requestValidated → store', () => {
  it('preserves ALL backend fields through the schema → store pipeline', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify(fullReport), {status: 200, headers: {'Content-Type': 'application/json'}}),
    )

    const {requestValidated} = await import('@/api/client')
    const {getAnalysisReportSchema} = await import('@/api/schemas')

    const report = await requestValidated('/api/analysis/analyze', await getAnalysisReportSchema())

    expect(report.flowId).toBe('flow-abc-123')
    expect(report.generatedAt).toBe('2024-07-12T10:30:00Z')
    expect(report.durationMs).toBe(42)

    expect(report.flowName).toBe('Customer Sync Flow')
    expect(report.metrics).toBeDefined()
    expect((report as {metrics?: {healthScore?: number}}).metrics?.healthScore).toBe(73)
    expect((report as {groups?: unknown[]}).groups).toBeDefined()
    expect((report as {groups?: unknown[]}).groups).toHaveLength(1)
    expect((report as {ruleProfiles?: unknown[]}).ruleProfiles).toBeDefined()
    expect((report as {ruleProfiles?: unknown[]}).ruleProfiles).toHaveLength(2)

    const f0 = report.findings[0]!
    expect(f0.fingerprint).toBe('hardcoded-credential:b1:ApiKey')
    expect(f0.confidence).toBe('high')
    expect(f0.autoFix).toBe('replace-with-variable')
    expect(f0.autoFixHint).toContain('vault')
    expect(f0.metadata).toEqual({property: 'Value', variable: 'ApiKey'})

    const f1 = report.findings[1]!
    expect(f1.autoFix).toBe('wrap-error-handler')

    expect((report.stats as {suppressed?: number}).suppressed).toBe(0)
    expect(report.stats.blocksAnalyzed).toBe(15)

    useAnalysisStore.getState().setReport('flow-abc-123', report)
    const stored = useAnalysisStore.getState().reports.get('flow-abc-123')
    expect(stored).toBeDefined()
    expect(stored!.findings).toHaveLength(2)
    expect(stored!.findings[0]!.autoFix).toBe('replace-with-variable')
    expect(stored!.findings[0]!.fingerprint).toBe('hardcoded-credential:b1:ApiKey')
    expect((stored as {metrics?: {healthScore?: number}}).metrics?.healthScore).toBe(73)
    expect((stored as {groups?: unknown[]}).groups).toHaveLength(1)
  })
})
