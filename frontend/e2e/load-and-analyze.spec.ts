import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

test.describe('Load flow and analyze', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/flow/upload', route =>
      route.fulfill({status: 200, json: mockFlowDocument()}),
    )
    await page.route('**/api/analysis/analyze', route =>
      route.fulfill({status: 200, json: mockAnalysisReport()}),
    )
    await page.route('**/api/dashboard/home', route =>
      route.fulfill({status: 200, json: mockDashboardHome()}),
    )
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('open file via sidebar loads flow into editor', async ({page}) => {
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
  })

  test('run analysis shows findings in inspector', async ({page}) => {
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

    const findingsTab = page.locator('button[title="Findings"]')
    if (await findingsTab.count() > 0) {
      await findingsTab.first().click()
    }

    const runBtn = page.locator('button:has-text("Run Analysis")')
    if (await runBtn.count() > 0) {
      await runBtn.click()
    }

    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible({timeout: 10000})
  })
})
