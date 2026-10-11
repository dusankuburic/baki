import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

test.describe('Filter findings', () => {
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

  test('severity filter: clicking Errors shows only error findings', async ({page}) => {
    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible()
    await expect(page.locator('text=Unhandled error').first()).toBeVisible()

    await page.locator('button:has-text("Warnings")').first().click()
    await page.waitForTimeout(500)

    await expect(page.locator('text=Unhandled error')).toHaveCount(0, {timeout: 5000})

    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible()

    await page.locator('button:has-text("Warnings")').first().click()
    await page.waitForTimeout(500)
    await expect(page.locator('text=Unhandled error').first()).toBeVisible({timeout: 5000})
  })

  test('search filter narrows findings by text', async ({page}) => {
    const searchInput = page.locator('input[placeholder="Search findings..."]')
    await expect(searchInput).toBeVisible({timeout: 5000})
    await searchInput.fill('Hardcoded')

    await page.waitForTimeout(500)

    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible()
    await expect(page.locator('text=Unhandled error')).toHaveCount(0, {timeout: 5000})
    await expect(page.locator('text=Unused variable')).toHaveCount(0, {timeout: 5000})

    await searchInput.fill('')
    await page.waitForTimeout(500)

    await expect(page.locator('text=Unhandled error').first()).toBeVisible({timeout: 5000})
  })

  test('category filter narrows findings', async ({page}) => {
    await page.locator('button:has-text("Reliability")').first().click()
    await page.locator('button:has-text("Style")').first().click()
    await page.waitForTimeout(500)

    await expect(page.locator('text=Hardcoded credential').first()).toBeVisible()

    await expect(page.locator('text=Unhandled error')).toHaveCount(0, {timeout: 5000})
    await expect(page.locator('text=Missing timeout')).toHaveCount(0, {timeout: 5000})

    await expect(page.locator('text=Unused variable')).toHaveCount(0, {timeout: 5000})
  })
})
