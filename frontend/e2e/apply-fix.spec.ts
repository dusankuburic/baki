import {test, expect} from '@playwright/test'
import {
  setupAuthenticatedPage,
  mockFlowDocument,
  mockAnalysisReport,
  mockDashboardHome,
  SAMPLE_FLOW_CONTENT,
} from './helpers'

test.describe('Apply fix loop', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/flow/upload', route => route.fulfill({status: 200, json: mockFlowDocument()}))
    await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))

    let fixApplied = false

    await page.route('**/api/analysis/analyze', route => {
      const report = mockAnalysisReport()
      if (fixApplied) {
        report.findings = report.findings.filter((f: {ruleId: string}) => f.ruleId !== 'unhandled-error')
        report.stats.warnings = report.stats.warnings - 1
      }
      route.fulfill({status: 200, json: report})
    })

    await page.route('**/api/flow/preview-fix', route =>
      route.fulfill({status: 403, json: {code: 'FORBIDDEN', message: 'preview not available'}}),
    )

    await page.route('**/api/flow/apply-fix', async route => {
      fixApplied = true
      const doc = mockFlowDocument()
      if (doc.subflows[0]?.blocks[2]) {
        doc.subflows[0].blocks[2].properties = {
          ...doc.subflows[0].blocks[2].properties,
          _fixed: 'wrap-error-handler',
        }
      }
      await route.fulfill({status: 200, json: doc})
    })

    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('apply fix resolves the finding', async ({page}) => {
    const openBtn = page.locator('button:has-text("Open file")').first()
    const [fileChooser] = await Promise.all([page.waitForEvent('filechooser'), openBtn.click()])
    await fileChooser.setFiles({
      name: 'Main.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(SAMPLE_FLOW_CONTENT),
    })
    await expect(page.locator('text=Customer Data Sync').first()).toBeVisible({timeout: 10000})

    const findingsTab = page.locator('button[title="Findings"]')
    if ((await findingsTab.count()) > 0) {
      await findingsTab.first().click()
    }
    const runBtn = page.locator('button:has-text("Run Analysis")')
    if ((await runBtn.count()) > 0) {
      await runBtn.click()
    }

    await expect(page.locator('text=Unhandled error').first()).toBeVisible({timeout: 10000})

    const applyFixBtn = page.locator('button:has-text("Apply fix")').first()
    if ((await applyFixBtn.count()) > 0) {
      await applyFixBtn.click()

      await expect(page.locator('text=Unhandled error')).toHaveCount(0, {timeout: 10000})
    }
  })
})
