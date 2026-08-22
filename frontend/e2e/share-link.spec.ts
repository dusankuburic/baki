import {test, expect} from '@playwright/test'

/**
 * Share-link happy path (the smoke spec only covers the negative cases).
 * Mocks /api/shared with a valid token and asserts the unauthenticated
 * report viewer renders the flow name and findings.
 */

const sharedPayload = {
  flowId: 'flow-shared',
  flowName: 'Invoices Pipeline',
  report: {
    flowId: 'flow-shared',
    flowName: 'Invoices Pipeline',
    generatedAt: new Date().toISOString(),
    durationMs: 12,
    stats: {errors: 1, warnings: 1, info: 0, blocksAnalyzed: 4, rulesRun: 29},
    findings: [
      {
        id: 'F-001',
        fingerprint: 'hardcoded-credential:s1',
        ruleId: 'hardcoded-credential',
        severity: 'error',
        confidence: 'high',
        title: 'Hardcoded credential',
        description: 'AWS access key detected',
        blockId: 'b1',
        subflowId: 'sf-main',
        category: 'Security',
        suggestion: 'Move the credential to a secure store',
      },
      {
        id: 'F-002',
        fingerprint: 'unused-variable:s2',
        ruleId: 'unused-variable',
        severity: 'warning',
        confidence: 'high',
        title: 'Unused variable',
        description: 'DebugFlag is set but never read',
        blockId: 'b2',
        subflowId: 'sf-main',
        category: 'Style',
      },
    ],
    metrics: {
      subflows: [],
      totalBlocks: 4,
      totalVariables: 2,
      maxCyclomatic: 2,
      avgCyclomatic: 2,
      maxCognitive: 1,
      avgCognitive: 1,
      healthScore: 58,
      variableDensity: 0.5,
      subflowCount: 1,
    },
  },
}

test.describe('Shared report viewer (happy path)', () => {
  test.beforeEach(async ({page}) => {
    await page.route('**/api/shared**', async route => {
      const url = new URL(route.request().url())
      if (url.searchParams.get('token') === 'valid-token-123') {
        await route.fulfill({status: 200, json: sharedPayload})
      } else {
        await route.fulfill({status: 404, json: {error: 'invalid or expired link'}})
      }
    })
  })

  test('renders the shared report for a valid token without login', async ({page}) => {
    await page.goto('/shared?token=valid-token-123')

    // Flow name header
    await expect(page.locator('text=Invoices Pipeline').first()).toBeVisible({timeout: 10_000})
    // Findings render with severity styling
    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible()
    await expect(page.locator('text=Unused variable').first()).toBeVisible()
  })

  test('an invalid token still shows the error state', async ({page}) => {
    await page.goto('/shared?token=wrong-token')
    await expect(page.locator('text=Invalid or expired link').first()).toBeVisible({timeout: 10_000})
  })

  test('the shared view renders outside the app shell (no sidebar navigation)', async ({page}) => {
    await page.goto('/shared?token=valid-token-123')
    await expect(page.locator('text=Invoices Pipeline').first()).toBeVisible({timeout: 10_000})
    // The unauthenticated viewer must not expose the authenticated nav.
    await expect(page.getByRole('button', {name: 'Analytics'})).toHaveCount(0)
  })
})
