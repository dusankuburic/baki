import {test, expect, Page} from '@playwright/test'
import {
  setupAuthenticatedPage,
  mockFlowDocument,
  mockAnalysisReport,
  mockDashboardHome,
  SAMPLE_FLOW_CONTENT,
} from './helpers'

/**
 * Flow toolbar E2E. The toolbar lives at the top of the main pane, whose
 * width the sidebar and inspector set: its controls used to be pushed
 * outside a narrow pane and clipped, the "Source" button floated over the
 * export icons, and the fullscreen button could only enter fullscreen.
 */

function flowWithIssues() {
  const doc = mockFlowDocument() as Record<string, unknown>
  doc.parseErrors = Array.from({length: 11}, (_, i) => ({
    line: 10 + i,
    column: 3,
    message: `Unexpected token at line ${10 + i}`,
    severity: 'error',
    snippet: 'WebAutomation.ClickLink BrowserInstance: %Browser%',
  }))
  return doc
}

async function openFlow(page: Page) {
  await setupAuthenticatedPage(page)
  await page.route('**/api/flow/upload', route => route.fulfill({status: 200, json: flowWithIssues()}))
  await page.route('**/api/analysis/analyze', route => route.fulfill({status: 200, json: mockAnalysisReport()}))
  await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  await page.route('**/api/analysis/dashboard', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  await page.route('**/api/flow/source?*', route => route.fulfill({status: 200, json: {source: SAMPLE_FLOW_CONTENT}}))
  await page.goto('/')
  await page.waitForLoadState('networkidle')
  const [fileChooser] = await Promise.all([
    page.waitForEvent('filechooser'),
    page.locator('button:has-text("Open file")').first().click(),
  ])
  await fileChooser.setFiles({name: 'Main.txt', mimeType: 'text/plain', buffer: Buffer.from(SAMPLE_FLOW_CONTENT)})
  await expect(page.getByRole('radio', {name: 'Block'})).toBeVisible({timeout: 10_000})
}

// Every control must stay inside the toolbar's box: no sideways overflow,
// and the rightmost button's right edge within the toolbar's right edge.
function toolbarMetrics(page: Page) {
  return page.evaluate(() => {
    const bar = document.querySelector<HTMLElement>('.main-toolbar')!
    const box = bar.getBoundingClientRect()
    const buttons = Array.from(bar.querySelectorAll<HTMLElement>('button')).filter(b => b.offsetParent !== null)
    const rightMost = Math.max(...buttons.map(b => b.getBoundingClientRect().right))
    const leftMost = Math.min(...buttons.map(b => b.getBoundingClientRect().left))
    const label = bar.querySelector<HTMLElement>('.seg-label')
    return {
      width: Math.round(box.width),
      overflow: bar.scrollWidth - bar.clientWidth,
      spill: Math.max(0, Math.round(rightMost - box.right)),
      leftSpill: Math.max(0, Math.round(box.left - leftMost)),
      labelsShown: !!label && getComputedStyle(label).display !== 'none',
    }
  })
}

test.describe('Flow toolbar', () => {
  test('keeps every control inside the pane at any width, folding the extras into More', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 900})
    await openFlow(page)

    // Wide pane: labelled view switcher, inline export buttons, no More menu.
    let m = await toolbarMetrics(page)
    expect(m.overflow).toBe(0)
    expect(m.spill).toBe(0)
    expect(m.labelsShown).toBe(true)
    await expect(page.getByRole('button', {name: 'Export PDF'})).toBeVisible()
    await expect(page.getByRole('button', {name: 'More actions'})).toBeHidden()

    // The source view sits in the switcher, level with the other views.
    const sourceBox = (await page.getByRole('radio', {name: 'Source'}).boundingBox())!
    const blockBox = (await page.getByRole('radio', {name: 'Block'}).boundingBox())!
    expect(Math.abs(sourceBox.y - blockBox.y)).toBeLessThanOrEqual(1)
    const exportBox = (await page.getByRole('button', {name: 'Export HTML'}).boundingBox())!
    expect(sourceBox.x + sourceBox.width).toBeLessThan(exportBox.x)

    // ~594px pane: icons only, still every control inline and inside the box.
    await page.setViewportSize({width: 1200, height: 900})
    await expect.poll(async () => (await toolbarMetrics(page)).labelsShown).toBe(false)
    m = await toolbarMetrics(page)
    expect(m.overflow).toBe(0)
    expect(m.spill).toBe(0)
    await expect(page.getByRole('radio', {name: 'Local Map'})).toBeVisible()
    await expect(page.getByRole('button', {name: 'Export PDF'})).toBeVisible()

    // ~394px pane: the exports fold into More; nothing is clipped.
    await page.setViewportSize({width: 1000, height: 900})
    await expect(page.getByRole('button', {name: 'More actions'})).toBeVisible()
    await expect(page.getByRole('button', {name: 'Export PDF'})).toBeHidden()
    m = await toolbarMetrics(page)
    expect(m.overflow).toBe(0)
    expect(m.spill).toBe(0)
    expect(m.leftSpill).toBe(0)
    await expect(page.getByRole('button', {name: 'Fullscreen'})).toBeVisible()
    await page.getByRole('button', {name: 'More actions'}).click()
    await expect(page.getByRole('menuitem', {name: 'Export PDF'})).toBeVisible()
    await expect(page.getByRole('menuitem', {name: 'Export HTML'})).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(page.getByRole('menu')).toBeHidden()

    // The parse banner keeps to one line as well.
    const banner = page.locator('.parse-banner')
    await expect(banner).toBeVisible()
    const bannerRow = await banner.locator(':scope > div').first().boundingBox()
    expect(bannerRow!.height).toBeLessThan(36)
  })

  test('fullscreen toggles back and mirrors the document state', async ({page}) => {
    await page.setViewportSize({width: 1400, height: 900})
    await openFlow(page)
    const isFullscreen = () => page.evaluate(() => !!document.fullscreenElement)

    await page.getByRole('button', {name: 'Fullscreen'}).click()
    await expect.poll(isFullscreen).toBe(true)
    await expect(page.getByRole('button', {name: 'Exit fullscreen'})).toBeVisible()

    await page.getByRole('button', {name: 'Exit fullscreen'}).click()
    await expect.poll(isFullscreen).toBe(false)
    await expect(page.getByRole('button', {name: 'Fullscreen'})).toBeVisible()

    // Leaving by another route (Escape, the browser's own controls) flips
    // the button back too.
    await page.getByRole('button', {name: 'Fullscreen'}).click()
    await expect(page.getByRole('button', {name: 'Exit fullscreen'})).toBeVisible()
    await page.evaluate(() => document.exitFullscreen())
    await expect(page.getByRole('button', {name: 'Fullscreen'})).toBeVisible()
  })

  test('the Source view opens the raw editor inline and another view closes it', async ({page}) => {
    await page.setViewportSize({width: 1400, height: 900})
    await openFlow(page)

    await page.getByRole('radio', {name: 'Source'}).click()
    await expect(page.getByText('Source Editor')).toBeVisible()
    await expect(page.getByRole('radio', {name: 'Source'})).toBeChecked()
    await expect(page.getByRole('button', {name: 'Complexity Map'})).toBeHidden()

    await page.getByRole('radio', {name: 'Block'}).click()
    await expect(page.getByText('Source Editor')).toBeHidden()
    await expect(page.getByRole('radio', {name: 'Block'})).toBeChecked()
  })
})
