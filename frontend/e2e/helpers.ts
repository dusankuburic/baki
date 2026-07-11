import {Page, Route} from '@playwright/test'

/**
 * Shared E2E test helpers: auth bypass, boot-call stubs, and mock-data
 * factories. Tests call setupAuthenticatedPage() in beforeEach to get an
 * app shell that past the login gate, then add their own route mocks for
 * the specific flow under test.
 */

// --- Auth bypass + boot stubs ---

export async function setupAuthenticatedPage(page: Page) {
  // 1. Preset a fake refresh token so ProtectedRoute's loadFromStorage proceeds
  //    past the login gate. A non-JWT string passes the local expiry check
  //    (decodeJwtPayload returns null → isJwtExpired returns false).
  await page.addInitScript(() => {
    localStorage.setItem('auth_refresh_token', 'fake-non-jwt-token')
  })

  // 2. Mock auth + boot endpoints. These must be fulfilled before the first
  //    page.goto so no real network call is attempted.
  const stubs: Record<string, unknown> = {
    '/api/local-config': {mode: 'cloud'},
    '/api/auth/refresh': {accessToken: 'fake-access-token', expiresAt: '2099-01-01T00:00:00Z'},
    '/api/auth/me': {id: 'user-1', email: 'tester@example.com', role: 'admin', displayName: 'Tester'},
    '/api/system/info': {
      version: 'test', platform: 'linux', arch: 'x86', buildDate: '', gitCommit: '',
      capabilities: {sessionAnalytics: false},
    },
    '/api/system/settings': {},
    '/api/flow/recent': [],
    '/api/orgs/': [],
  }

  await page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    for (const [path, body] of Object.entries(stubs)) {
      if (url.pathname === path) {
        await route.fulfill({status: 200, json: body})
        return
      }
    }
    // Let un-mocked routes pass through (they'll hit the dev server and 404,
    // which is fine — the app handles errors gracefully).
    await route.fallback()
  })
}

// --- Mock data factories ---

export function mockFlowDocument() {
  return {
    id: 'flow-1',
    name: 'Customer Data Sync (sample)',
    filePath: '',
    subflows: [{
      id: 'sf-main',
      name: 'Main',
      blocks: [
        {id: 'b1', name: 'Variables.SetVariable', type: 'ACTION', rawType: 'ACTION', indent: 0, lineNumber: 2, children: [], properties: {Name: 'ApiKey', Value: "'AKIAIOSFODNN7EXAMPLE'"}, variables: ['ApiKey'], subflowId: 'sf-main'},
        {id: 'b2', name: 'Variables.SetVariable', type: 'ACTION', rawType: 'ACTION', indent: 0, lineNumber: 3, children: [], properties: {Name: 'DebugFlag', Value: 'True'}, variables: ['DebugFlag'], subflowId: 'sf-main'},
        {id: 'b3', name: 'HTTP.InvokeUrl', type: 'ACTION', rawType: 'ACTION', indent: 0, lineNumber: 4, children: [], properties: {Method: 'GET', Url: "'https://api.example.com/customers'"}, variables: ['Response'], subflowId: 'sf-main'},
      ],
      variables: [
        {name: 'ApiKey', type: 'string', scope: 'flow'},
        {name: 'DebugFlag', type: 'boolean', scope: 'flow'},
      ],
    }],
    metadata: {blockCount: 3, subflowCount: 1, maxDepth: 1, parsedAt: new Date().toISOString(), fileSize: 500, rawLineCount: 10},
  }
}

export const SAMPLE_FLOW_CONTENT = `#Region "Main"
Variables.SetVariable Name: %ApiKey% Value: 'AKIAIOSFODNN7EXAMPLE'
Variables.SetVariable Name: %DebugFlag% Value: True
HTTP.InvokeUrl Method: GET Url: 'https://api.example.com/customers' Accept: 'application/json' => %Response%
LOOP FROM 1 TO 50 STEP 1
    WebAutomation.ClickLink BrowserInstance: %Browser% Link: 'next page'
    IF %Response% = '' THEN
        Display.ShowMessageBox Message: 'No data returned from API'
    END
END
Text.WriteToFile TextToWrite: %Response% FilePath: 'C:\\Reports\\customers.txt' IfFileExists: Overwrite
#EndRegion
`

export function mockFindings() {
  return [
    {id: 'F-001', fingerprint: 'hardcoded-credential:b1', ruleId: 'hardcoded-credential', severity: 'error', confidence: 'high', title: 'Hardcoded credential', description: 'AWS access key detected in ApiKey variable', blockId: 'b1', subflowId: 'sf-main', category: 'Security', suggestion: 'Move the credential to a secure store'},
    {id: 'F-002', fingerprint: 'unused-variable:b2', ruleId: 'unused-variable', severity: 'warning', confidence: 'high', title: 'Unused variable: DebugFlag', description: 'DebugFlag is set but never read', blockId: 'b2', subflowId: 'sf-main', category: 'Style', suggestion: 'Remove the variable if it is not needed'},
    {id: 'F-003', fingerprint: 'unhandled-error:b3', ruleId: 'unhandled-error', severity: 'warning', confidence: 'high', title: 'Unhandled error on HTTP action', description: 'HTTP.InvokeUrl has no On Block Error handler', blockId: 'b3', subflowId: 'sf-main', category: 'Reliability', suggestion: 'Add an error handler', autoFix: 'wrap-error-handler'},
    {id: 'F-004', fingerprint: 'missing-timeout:b3', ruleId: 'missing-timeout', severity: 'info', confidence: 'medium', title: 'Missing timeout', description: 'HTTP.InvokeUrl has no timeout configured', blockId: 'b3', subflowId: 'sf-main', category: 'Reliability'},
  ]
}

export function mockAnalysisReport() {
  return {
    flowId: 'flow-1',
    flowName: 'Customer Data Sync (sample)',
    generatedAt: new Date().toISOString(),
    durationMs: 5,
    stats: {errors: 1, warnings: 2, info: 1, blocksAnalyzed: 3, rulesRun: 29},
    findings: mockFindings(),
    metrics: {
      subflows: [{subflowId: 'sf-main', subflowName: 'Main', blockCount: 3, cyclomaticComplexity: 2, cognitiveComplexity: 1, maxNestingDepth: 1, variableCount: 2, fanIn: 0, fanOut: 0}],
      totalBlocks: 3, totalVariables: 2, maxCyclomatic: 2, avgCyclomatic: 2, maxCognitive: 1, avgCognitive: 1, healthScore: 65, variableDensity: 0.67, subflowCount: 1,
    },
  }
}

export function mockDashboardHome() {
  return {
    greeting: 'Welcome back',
    overview: {totalFlows: 0, totalFindings: 0, avgHealth: 0, criticalFindings: 0},
    tokenUsage: [],
    recentFlows: [],
    findings: {errors: 0, warnings: 0, info: 0},
    isCloud: true,
    healthTrend: [],
    costByProvider: [],
    ruleFrequency: [],
    activity: [],
    complexity: [],
    security: {criticalIssues: 0, hardcodedCredentials: 0, exposedSecrets: 0},
    severityTrend: [],
    confidenceDist: [],
    healthBuckets: [],
    fixability: {fixable: 0, unfixable: 0},
    workflow: {automated: 0, manual: 0},
  }
}
