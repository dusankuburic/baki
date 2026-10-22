import {test, expect, type Page} from '@playwright/test'
import {setupAuthenticatedPage, mockFlowDocument, mockAnalysisReport, mockDashboardHome, SAMPLE_FLOW_CONTENT} from './helpers'

/**
 * Resizing a pane must not redraw a graph.
 *
 * With React Flow the graph is DOM inside one transformed element: the container's size feeds
 * nothing but `fitView`, which runs on mount and on an explicit request, never on a resize. So the
 * contract a divider drag or a window resize has to keep is simply that the viewport's transform
 * never changes while it happens — no refit, no reflowed graph, nothing to flicker. These tests
 * replaced four that measured Cytoscape's canvas reallocations, the freeze and the snapshot shield,
 * none of which exist any more.
 */

const SUBFLOW_GRAPH = () => {
  const nodes = Array.from({length: 25}, (_, i) => ({
    id: i === 0 ? 'sf-main' : `sf-${i}`,
    label: i === 0 ? 'Main' : `Subflow ${i}`,
    type: 'subflow',
    blockCount: 20 + i * 7,
    errorCount: i % 3,
    warnCount: i % 4,
  }))
  const edges = nodes.slice(1).map(n => ({source: 'sf-main', target: n.id, callCount: 1 + (n.blockCount % 3)}))
  return {nodes, edges}
}

async function openFlow(page: Page) {
  await setupAuthenticatedPage(page)
  await page.route('**/api/flow/upload', route => route.fulfill({status: 200, json: mockFlowDocument()}))
  await page.route('**/api/analysis/analyze', route => route.fulfill({status: 200, json: mockAnalysisReport()}))
  await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  await page.route('**/api/analysis/graph', route => route.fulfill({status: 200, json: SUBFLOW_GRAPH()}))
  await page.goto('/')
  await page.waitForLoadState('networkidle')

  const openBtn = page.locator('button:has-text("Open file")').first()
  const [fileChooser] = await Promise.all([page.waitForEvent('filechooser'), openBtn.click()])
  await fileChooser.setFiles({name: 'Main.txt', mimeType: 'text/plain', buffer: Buffer.from(SAMPLE_FLOW_CONTENT)})
  await expect(page.locator('text=Customer Data Sync').first()).toBeVisible({timeout: 10000})
}

async function waitForGraph(page: Page) {
  await expect(page.locator('.react-flow__node').first()).toBeVisible({timeout: 10000})
  // The first fit lands in an effect right after the nodes are measured; wait for it to settle so
  // the transform recorded below is the one the drag must not disturb.
  await page.waitForFunction(() => {
    const el = document.querySelector<HTMLElement>('.react-flow__viewport')
    return !!el && el.style.transform.includes('scale')
  })
  await page.waitForTimeout(150)
}

/** Record the viewport's transform and start counting every change to it. */
async function watchViewport(page: Page) {
  await page.evaluate(() => {
    const el = document.querySelector<HTMLElement>('.react-flow__viewport')
    if (!el) throw new Error('react flow viewport missing')
    const scope = window as unknown as {__vp?: {changes: number; seen: string[]}; __vpObserver?: MutationObserver}
    scope.__vpObserver?.disconnect()
    scope.__vp = {changes: 0, seen: [el.style.transform]}
    const observer = new MutationObserver(records => {
      const state = scope.__vp!
      state.changes += records.length
      if (!state.seen.includes(el.style.transform)) state.seen.push(el.style.transform)
    })
    observer.observe(el, {attributes: true, attributeFilter: ['style']})
    scope.__vpObserver = observer
  })
}

async function viewportChanges(page: Page) {
  return page.evaluate(() => {
    const scope = window as unknown as {__vp?: {changes: number; seen: string[]}}
    return scope.__vp ?? {changes: 0, seen: []}
  })
}

/** Screen box of the first node, and of the pane it lives in. */
async function geometry(page: Page) {
  return page.evaluate(() => {
    const node = document.querySelector<HTMLElement>('.react-flow__node')
    const pane = document.querySelector<HTMLElement>('.react-flow')
    if (!node || !pane) throw new Error('graph missing')
    const n = node.getBoundingClientRect()
    const p = pane.getBoundingClientRect()
    return {nodeX: n.x, nodeY: n.y, paneX: p.x, paneWidth: p.width}
  })
}

async function dragDivider(page: Page, name: string, by: number) {
  const divider = page.getByRole('separator', {name})
  await expect(divider).toBeVisible()
  const box = await divider.boundingBox()
  if (!box) throw new Error(`${name} has no box`)
  const y = box.y + box.height / 2
  const from = box.x + box.width / 2

  await page.mouse.move(from, y)
  await page.mouse.down()
  const steps = 12
  const samples: Array<{nodeX: number; paneWidth: number}> = []
  for (let i = 1; i <= steps; i++) {
    await page.mouse.move(from + (by * i) / steps, y)
    if (i % 4 === 0) samples.push(await geometry(page))
  }
  await page.mouse.up()
  await page.waitForTimeout(250)
  return samples
}

test('an inspector drag leaves the graph where it is, with no refit', async ({page}) => {
  await openFlow(page)
  await page.locator('button', {hasText: 'Graph'}).first().click()
  await waitForGraph(page)

  const before = await geometry(page)
  await watchViewport(page)

  const samples = await dragDivider(page, 'Resize inspector', -140)

  // Dragging the divider left widens the inspector, so the main pane loses that width on its right
  // edge while the graph itself does not move at all — it is simply clipped, which is the whole
  // point: no refit, no redraw, not during the drag and not on release.
  for (const sample of samples) expect(Math.abs(sample.nodeX - before.nodeX)).toBeLessThan(1)
  const after = await geometry(page)
  expect(Math.abs(after.nodeX - before.nodeX)).toBeLessThan(1)
  expect(Math.abs(after.nodeY - before.nodeY)).toBeLessThan(1)
  expect(after.paneWidth).toBeLessThan(before.paneWidth - 100)

  const changes = await viewportChanges(page)
  expect(changes.changes).toBe(0)
  expect(changes.seen).toHaveLength(1)
})

test('a sidebar drag moves the graph with its pane, with no refit', async ({page}) => {
  await openFlow(page)
  await page.locator('button', {hasText: 'Graph'}).first().click()
  await waitForGraph(page)

  const before = await geometry(page)
  await watchViewport(page)

  await dragDivider(page, 'Resize sidebar', 120)

  // The pane's left edge moves, so the graph moves with it by exactly the same amount: a layout
  // shift, not a redraw.
  const after = await geometry(page)
  const paneShift = after.paneX - before.paneX
  expect(paneShift).toBeGreaterThan(50)
  expect(Math.abs(after.nodeX - before.nodeX - paneShift)).toBeLessThan(1)

  const changes = await viewportChanges(page)
  expect(changes.changes).toBe(0)
  expect(changes.seen).toHaveLength(1)
})

test('a window resize leaves the graph viewport untouched', async ({page}) => {
  await openFlow(page)
  await page.locator('button', {hasText: 'Graph'}).first().click()
  await waitForGraph(page)

  await watchViewport(page)
  const before = await geometry(page)

  await page.setViewportSize({width: 1100, height: 700})
  await page.waitForTimeout(200)
  await page.setViewportSize({width: 1400, height: 900})
  await page.waitForTimeout(300)

  const changes = await viewportChanges(page)
  expect(changes.changes).toBe(0)
  expect(changes.seen).toHaveLength(1)
  const after = await geometry(page)
  expect(Math.abs(after.nodeY - before.nodeY)).toBeLessThan(1)
})

test('the local map holds still through a divider drag', async ({page}) => {
  await openFlow(page)
  await page.getByRole('radio', {name: 'Local Map', exact: true}).click()
  await waitForGraph(page)

  const before = await geometry(page)
  await watchViewport(page)

  const samples = await dragDivider(page, 'Resize inspector', -140)
  for (const sample of samples) expect(Math.abs(sample.nodeX - before.nodeX)).toBeLessThan(1)

  const changes = await viewportChanges(page)
  expect(changes.changes).toBe(0)
  expect(changes.seen).toHaveLength(1)
})
