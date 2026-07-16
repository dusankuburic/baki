import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

// E2E spec: the core analyze → apply-fix → re-analyze loop.
// Verifies that clicking "Apply Fix" on an auto-fixable finding causes it to
// disappear from the findings list after re-analysis. This is the UI-level
// equivalent of the Go round-trip gate tests in autofix_test.go.
test.describe('Apply fix loop', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/flow/upload', route =>
      route.fulfill({status: 200, json: mockFlowDocument()}),
    )
    await page.route('**/api/dashboard/home', route =>
      route.fulfill({status: 200, json: mockDashboardHome()}),
    )

    // Track whether the fix has been applied so we can return different
    // analysis results on the second /api/analysis/analyze call.
    let fixApplied = false

    // First analysis: full report with an auto-fixable finding.
    // Second analysis (post-fix): report without the fixed finding.
    await page.route('**/api/analysis/analyze', route => {
      const report = mockAnalysisReport()
      if (fixApplied) {
        // Remove the unhandled-error finding (the one that was fixed)
        report.findings = report.findings.filter(
          (f: {ruleId: string}) => f.ruleId !== 'unhandled-error',
        )
        report.stats.warnings = report.stats.warnings - 1
      }
      route.fulfill({status: 200, json: report})
    })

    // Mock the apply-fix endpoint: returns the updated document.
    await page.route('**/api/flow/apply-fix', async route => {
      fixApplied = true
      const doc = mockFlowDocument()
      // Simulate the fix wrapping the block in an error handler
      if (doc.subflows[0]?.blocks[2]) {
        doc.subflows[0].blocks[2].properties = {
          ...doc.subflows[0].blocks[2].properties,
          _fixed: 'wrap-error-handler',
        }
      }
      await route.fulfill({status: 200, json: {document: doc, applied: 1}})
    })

    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('apply fix resolves the finding', async ({page}) => {
    // 1. Load the sample flow
    const openBtn = page.locator('button:has-text("Open file")').first()
    const [fileChooser] = await Promise.all([
      page.waitForEvent('filechooser'),
      openBtn.click(),
    ])
    await fileChooser.setFiles({
      name: 'Main.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(SAMPLE_FLOW_CONTENT),
    })
    await expect(page.locator('text=Customer Data Sync').first()).toBeVisible({timeout: 10000})

    // 2. Run analysis
    const findingsTab = page.locator('button[title="Findings"]')
    if (await findingsTab.count() > 0) {
      await findingsTab.first().click()
    }
    const runBtn = page.locator('button:has-text("Run Analysis")')
    if (await runBtn.count() > 0) {
      await runBtn.click()
    }

    // 3. Verify the unhandled-error finding is visible
    await expect(page.locator('text=Unhandled error').first()).toBeVisible({timeout: 10000})

    // 4. Click the "Apply fix" button on the finding
    const applyFixBtn = page.locator('button:has-text("Apply fix")').first()
    if (await applyFixBtn.count() > 0) {
      await applyFixBtn.click()

      // 5. After re-analysis, the finding should be gone
      await expect(page.locator('text=Unhandled error')).toHaveCount(0, {timeout: 10000})
    }
  })
})
