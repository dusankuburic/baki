// A complete GET /api/dashboard/analytics payload for tests: thirty days ending
// today, four flows, three rules in two categories, some triage, AI usage and
// activity. Shared by the unit tests and the Playwright specs, so both render
// the same page. Kept free of app imports so the e2e runner can load it.

export interface AnalyticsFixtureOptions {
  days?: number
  compare?: boolean
  flowsInScope?: number
  partial?: string[]
  derived?: boolean
}

function day(offset: number, from: Date): string {
  const d = new Date(from)
  d.setDate(d.getDate() + offset)
  return d.toISOString().slice(0, 10)
}

export function mockDashboardAnalytics(opts: AnalyticsFixtureOptions = {}) {
  const days = opts.days ?? 30
  const compare = opts.compare ?? true
  const flowsInScope = opts.flowsInScope ?? 4
  const today = new Date()
  today.setUTCHours(0, 0, 0, 0)
  const start = new Date(today)
  start.setDate(start.getDate() - (days - 1))
  const prevStart = new Date(start)
  prevStart.setDate(prevStart.getDate() - days)

  const point = (i: number, base: number, from: Date) => {
    const health = i < 2 ? null : Math.min(100, base + Math.round(Math.sin(i / 3) * 6))
    return {
      date: day(i, from),
      avgHealth: health,
      flowCount: health === null ? 0 : 4,
      runs: i % 5 === 0 ? 2 : i % 3 === 0 ? 1 : 0,
      findings: health === null ? 0 : 40 - Math.round(i / 3),
      errors: health === null ? 0 : 6,
      warnings: health === null ? 0 : 24 - Math.round(i / 4),
      info: health === null ? 0 : 10,
      autoFixable: health === null ? 0 : 12,
    }
  }
  const points = Array.from({length: days}, (_, i) => point(i, 74, start))
  const prevPoints = compare ? Array.from({length: days}, (_, i) => point(i, 68, prevStart)) : []
  const dateCounts = (n: number) =>
    Array.from({length: days}, (_, i) => ({date: day(i, start), count: i % 4 === 0 ? n : 0}))
  const stat = (value: number, prev: number | null, available = true) => ({
    available,
    value,
    prev: compare ? prev : null,
    spark: Array.from({length: 12}, (_, i) => Math.round(value * (0.8 + (i % 3) * 0.1))),
  })
  const flows = [
    {id: 'flow-1', name: 'Invoice sync', health: 42, findings: 14},
    {id: 'flow-2', name: 'Customer onboarding', health: 61, findings: 9},
    {id: 'flow-3', name: 'Nightly export', health: 83, findings: 5},
    {id: 'flow-4', name: 'Legacy loader', health: 91, findings: 2},
  ].slice(0, flowsInScope)

  return {
    applied: {
      scope: 'all' as const,
      flows: [] as string[],
      severity: [] as string[],
      category: [] as string[],
      rule: [] as string[],
      compare,
      tz: 'UTC',
      derived: opts.derived ?? false,
    },
    range: {
      preset: days === 7 ? '7d' : days === 90 ? '90d' : '30d',
      from: day(0, start),
      to: day(days - 1, start),
      days,
      ...(compare ? {prevFrom: day(0, prevStart), prevTo: day(days - 1, prevStart)} : {}),
    },
    summary: {
      asOf: day(days - 1, start),
      flowsInScope,
      unanalyzed: flowsInScope > 0 ? 1 : 0,
      avgHealth: stat(72, 66, flowsInScope > 0),
      openFindings: stat(30, 38, flowsInScope > 0),
      errors: stat(6, 9, flowsInScope > 0),
      warnings: stat(14, 19, flowsInScope > 0),
      info: stat(10, 10, flowsInScope > 0),
      autoFixable: stat(12, 15, flowsInScope > 0),
      analysesRun: stat(14, 11),
      flowsAnalyzed: stat(Math.max(0, flowsInScope - 1), Math.max(0, flowsInScope - 2)),
      resolved: stat(7, 4),
      aiSpend: stat(3.42, 2.1),
    },
    trend: {points, prevPoints},
    findings: {
      bySeverity: {error: 6, warning: 14, info: 10},
      byCategory: [
        {category: 'Reliability', count: 18, errors: 4, warnings: 10, info: 4},
        {category: 'Security', count: 8, errors: 2, warnings: 2, info: 4},
        {category: 'Other', count: 4, errors: 0, warnings: 2, info: 2},
      ],
      byRule: [
        {
          rule: 'missing-error-handling',
          name: 'Missing error handling',
          category: 'Reliability',
          severity: 'error',
          autoFixable: false,
          inCatalog: true,
          count: 12,
          flows: 3,
        },
        {
          rule: 'hardcoded-credential',
          name: 'Hardcoded credential',
          category: 'Security',
          severity: 'error',
          autoFixable: false,
          inCatalog: true,
          count: 8,
          flows: 2,
        },
        {
          rule: 'missing-timeout',
          name: 'Missing timeout',
          category: 'Reliability',
          severity: 'warning',
          autoFixable: true,
          inCatalog: true,
          count: 6,
          flows: 3,
        },
        {
          rule: 'custom-x',
          name: 'custom-x',
          category: 'Other',
          severity: '',
          autoFixable: false,
          inCatalog: false,
          count: 4,
          flows: 1,
        },
      ],
      byConfidence: {high: 18, medium: 9, low: 3},
      fixability: {available: 12, total: 30, autoFixableRules: 11, totalRules: 44},
    },
    flows: {
      healthBuckets: [
        {label: '0-20', lo: 0, hi: 20, count: 0},
        {label: '20-40', lo: 20, hi: 40, count: 0},
        {label: '40-60', lo: 40, hi: 60, count: 1},
        {label: '60-80', lo: 60, hi: 80, count: 1},
        {label: '80-100', lo: 80, hi: 100, count: 2},
      ],
      complexity: flows.map((f, i) => ({
        flowId: f.id,
        name: f.name,
        ownerId: 'user-1',
        blockCount: 40 + i * 35,
        findings: f.findings,
        errors: Math.round(f.findings / 3),
        warnings: Math.round(f.findings / 2),
        info: f.findings - Math.round(f.findings / 3) - Math.round(f.findings / 2),
        health: f.health,
        analyzedAt: new Date(Date.now() - (i + 1) * 3600e3).toISOString(),
      })),
      worst: flows.map((f, i) => ({
        flowId: f.id,
        name: f.name,
        ownerId: 'user-1',
        blockCount: 40 + i * 35,
        findings: f.findings,
        errors: Math.round(f.findings / 3),
        warnings: Math.round(f.findings / 2),
        info: f.findings - Math.round(f.findings / 3) - Math.round(f.findings / 2),
        health: f.health,
        analyzedAt: new Date(Date.now() - (i + 1) * 3600e3).toISOString(),
      })),
      movers: compare
        ? [
            {flowId: 'flow-1', name: 'Invoice sync', healthFrom: 58, healthTo: 42, delta: -16},
            {flowId: 'flow-3', name: 'Nightly export', healthFrom: 70, healthTo: 83, delta: 13},
          ]
        : [],
    },
    triage: {
      available: true,
      funnel: {open: 11, acknowledged: 4, in_progress: 3, resolved: 7, suppressed: 2},
      backlog: 18,
      mttrHours: 16.5,
      resolvedCount: 7,
      staleCount: 3,
      resolvedByDay: dateCounts(1),
    },
    ai: {
      scoped: 'user' as const,
      flowScoped: false,
      daily: Array.from({length: days}, (_, i) => ({
        date: day(i, start),
        tokensIn: i % 2 === 0 ? 1200 + i * 30 : 0,
        tokensOut: i % 2 === 0 ? 400 + i * 10 : 0,
        cost: i % 2 === 0 ? 0.12 : 0,
        calls: i % 2 === 0 ? 3 : 0,
      })),
      byProvider: [
        {provider: 'anthropic', cost: 2.4, tokensIn: 30000, tokensOut: 9000},
        {provider: 'openai', cost: 1.02, tokensIn: 12000, tokensOut: 4000},
      ],
      byModel: [
        {provider: 'anthropic', model: 'claude-sonnet-5', tokensIn: 30000, tokensOut: 9000, calls: 30, cost: 2.4},
        {provider: 'openai', model: 'gpt-5', tokensIn: 12000, tokensOut: 4000, calls: 15, cost: 1.02},
      ],
      totalCost: 3.42,
      prevCost: compare ? 2.1 : null,
      totalTokens: 55000,
    },
    activity: {
      events: [
        {
          action: 'flow.analyze',
          actor: {id: 'user-1', name: 'Tester', isMe: true},
          resourceType: 'flow',
          resourceId: 'flow-1',
          label: 'Invoice sync',
          createdAt: new Date(Date.now() - 3600e3).toISOString(),
        },
        {
          action: 'flow.share',
          actor: {id: 'user-2', name: 'Ada', isMe: false},
          resourceType: 'flow',
          resourceId: 'flow-2',
          label: 'Customer onboarding',
          createdAt: new Date(Date.now() - 7200e3).toISOString(),
        },
        {
          action: 'finding.triage',
          actor: {id: 'user-1', name: 'Tester', isMe: true},
          resourceType: 'flow',
          resourceId: 'flow-1',
          label: 'Invoice sync',
          createdAt: new Date(Date.now() - 86400e3).toISOString(),
        },
      ],
      byDay: dateCounts(3),
      byAction: [
        {action: 'flow.analyze', count: 9},
        {action: 'finding.triage', count: 4},
        {action: 'flow.share', count: 1},
      ],
    },
    security: {
      available: true,
      failedLogins24h: 1,
      securityFindings: 8,
      credentialFindings: 8,
      lockedAccounts: null as number | null,
    },
    facets: {
      severity: [
        {key: 'error', label: 'error', count: 6},
        {key: 'warning', label: 'warning', count: 14},
        {key: 'info', label: 'info', count: 10},
      ],
      category: [
        {key: 'Reliability', label: 'Reliability', count: 18},
        {key: 'Security', label: 'Security', count: 8},
        {key: 'Other', label: 'Other', count: 4},
      ],
      rule: [
        {key: 'missing-error-handling', label: 'Missing error handling', count: 12},
        {key: 'hardcoded-credential', label: 'Hardcoded credential', count: 8},
        {key: 'missing-timeout', label: 'Missing timeout', count: 6},
        {key: 'custom-x', label: 'custom-x', count: 4},
      ],
      flows: flows.map(f => ({
        id: f.id,
        name: f.name,
        health: f.health,
        findings: f.findings,
        analyzedAt: new Date(Date.now() - 3600e3).toISOString(),
      })),
      flowsTruncated: false,
    },
    partial: opts.partial ?? [],
    generatedAt: new Date().toISOString(),
  }
}
