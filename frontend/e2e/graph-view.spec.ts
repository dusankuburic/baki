import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

test('graph view renders the flow as a sized cytoscape canvas', async ({page}) => {
  await setupAuthenticatedPage(page)
  await page.route('**/api/flow/upload', route => route.fulfill({status: 200, json: mockFlowDocument()}))
  await page.route('**/api/analysis/analyze', route => route.fulfill({status: 200, json: mockAnalysisReport()}))
  await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  await page.goto('/')
  await page.waitForLoadState('networkidle')

  const openBtn = page.locator('button:has-text("Open file")').first()
  const [fileChooser] = await Promise.all([page.waitForEvent('filechooser'), openBtn.click()])
  await fileChooser.setFiles({name: 'Main.txt', mimeType: 'text/plain', buffer: Buffer.from(SAMPLE_FLOW_CONTENT)})
  await expect(page.locator('text=Customer Data Sync').first()).toBeVisible({timeout: 10000})

  await page.locator('button', {hasText: 'Graph'}).first().click()

  // Cytoscape injects an unlayered `.__________cytoscape_container {position:
  // relative}` stylesheet at runtime, which beats Tailwind v4's layered
  // `.absolute` and collapses the absolute inset-0 container to height 0 —
  // the graph renders blank. The assertion checks the MAIN canvas has real
  // height (the minimap canvas alone is only ~98px tall).
  await expect
    .poll(
      async () =>
        (await page.evaluate(() => Array.from(document.querySelectorAll('canvas')).map(c => c.clientHeight))).filter(
          h => h > 100,
        ).length,
      {timeout: 10000},
    )
    .toBeGreaterThan(0)
})
