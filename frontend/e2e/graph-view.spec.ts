import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

test('graph view renders the flow as positioned React Flow nodes', async ({page}) => {
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

  // The graph is DOM, so "it rendered" means laid-out nodes inside a sized pane — and the pane is
  // no longer at the mercy of a runtime stylesheet collapsing it to zero height, as the Cytoscape
  // container was.
  const nodes = page.locator('.react-flow__node')
  await expect(nodes.first()).toBeVisible({timeout: 10000})
  expect(await nodes.count()).toBeGreaterThan(1)

  const pane = await page.locator('.react-flow').first().boundingBox()
  expect(pane?.height ?? 0).toBeGreaterThan(100)

  // The graph is hidden until its first fit lands, so "ready" is what separates a fitted graph
  // from a blank pane.
  await expect(page.locator('[data-flow-ready="true"]').first()).toBeVisible()
  expect(await nodes.first().evaluate(el => getComputedStyle(el).opacity)).toBe('1')

  // Nodes carry their layout position, not a default stack at the origin, and the blocks are
  // joined by drawn edges.
  const positions = await page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>('.react-flow__node')).map(el => el.style.transform),
  )
  expect(new Set(positions).size).toBeGreaterThan(1)
  expect(await page.locator('.react-flow__edge-path').count()).toBeGreaterThan(0)

  // The minimap is SVG now, with a node for every graph node it can see.
  await expect(page.locator('.react-flow__minimap')).toBeVisible()
})

test('the toolbar exports the graph as a PNG', async ({page}) => {
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
  await expect(page.locator('.react-flow__node').first()).toBeVisible({timeout: 10000})

  // Cytoscape rasterized its own canvas; the DOM graph is rasterized with html-to-image, loaded
  // on demand when the export runs.
  const [download] = await Promise.all([
    page.waitForEvent('download', {timeout: 20000}),
    page.getByRole('button', {name: 'Export graph as PNG'}).click(),
  ])
  expect(download.suggestedFilename()).toMatch(/\.png$/)
})
