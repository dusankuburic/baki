import {test, expect, Page, Route} from '@playwright/test'
import {setupAuthenticatedPage} from './helpers'
import {chartLayoutMetrics, WIDTHS} from './chartChecks'
import {mockDashboardAnalytics} from '../src/testing/fixtures/dashboardAnalytics'

/**
 * Analytics page E2E. The page sends the filters as one canonical query,
 * every chip changes it, refetches keep the previous charts on screen, each
 * chart has a table twin, and — the layout being keyed to the main pane's
 * width — nothing overflows and no axis tick is clipped at any width.
 */

async function stubAnalytics(page: Page) {
  await setupAuthenticatedPage(page)
  const queries: URLSearchParams[] = []
  await page.route('**/api/dashboard/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/dashboard/analytics') {
      queries.push(url.searchParams)
      await route.fulfill({status: 200, json: mockDashboardAnalytics()})
      return
    }
    await route.fallback()
  })
  return queries
}

async function openAnalytics(page: Page) {
  await page.goto('/')
  await page.getByRole('button', {name: 'Analytics', exact: true}).first().click()
  await expect(page.getByRole('heading', {name: 'Analytics', level: 1})).toBeVisible({timeout: 10_000})
  await expect(page.getByRole('heading', {name: 'Overview'})).toBeVisible()
  await expect(page.locator('svg.recharts-surface').first()).toBeVisible()
}

function card(page: Page, title: string) {
  return page.locator('.chart-card', {has: page.getByRole('heading', {name: title, exact: true})})
}

async function settled(page: Page) {
  await expect(page.locator('.chart-card[aria-busy="true"]')).toHaveCount(0)
  await expect(page.locator('.analytics-progress.is-active')).toHaveCount(0)
}

// The shell never moves: the document has no scroll offset and no hidden
// scroll range (an absolutely positioned descendant used to give <html> a
// few thousand pixels of overflow, which the section jump then scrolled).
async function shellStill(page: Page) {
  const doc = await page.evaluate(() => ({
    top: document.documentElement.scrollTop,
    height: document.documentElement.scrollHeight,
    viewport: window.innerHeight,
  }))
  expect(doc.top).toBe(0)
  expect(doc.height).toBe(doc.viewport)
}

test.describe('Analytics page', () => {
  test('sends the filters as one query and reflects them in the chips and summary', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    const queries = await stubAnalytics(page)
    await openAnalytics(page)
    await settled(page)

    expect(queries.length).toBe(1)
    expect(queries[0].get('range')).toBe('30d')
    expect(queries[0].get('scope')).toBe('all')
    expect(queries[0].get('compare')).toBe('1')
    expect(queries[0].get('tz')).toBeTruthy()
    await expect(page.getByText(/^Showing Last 30 days · All my flows · vs previous period\.$/)).toBeVisible()

    await page.getByRole('radio', {name: '7 days'}).click()
    await expect.poll(() => queries.length).toBe(2)
    expect(queries[1].get('range')).toBe('7d')
    await settled(page)

    const errors = page.getByRole('button', {name: 'Errors, 6 findings'})
    await errors.click()
    await expect.poll(() => queries.length).toBe(3)
    expect(queries[2].get('severity')).toBe('error')
    await expect(errors).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByText(/Showing Last 7 days · All my flows · Errors · vs previous period\./)).toBeVisible()
    await settled(page)

    await page.getByRole('button', {name: 'Missing timeout, 6 findings'}).click()
    await expect.poll(() => queries.length).toBe(4)
    expect(queries[3].get('rule')).toBe('missing-timeout')
    expect(queries[3].get('severity')).toBe('error')

    await page.getByRole('button', {name: 'Reset filters'}).click()
    await expect.poll(() => queries.length).toBe(5)
    expect(queries[4].get('range')).toBe('30d')
    expect(queries[4].has('severity')).toBe(false)
    expect(queries[4].has('rule')).toBe(false)
  })

  test('custom ranges and the flow picker', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    const queries = await stubAnalytics(page)
    await openAnalytics(page)
    await settled(page)

    await page.getByRole('radio', {name: 'Custom'}).click()
    await page.getByLabel('From', {exact: true}).fill('2026-09-10')
    await page.getByLabel('To', {exact: true}).fill('2026-09-01')
    await page.getByRole('button', {name: 'Apply'}).click()
    await expect(page.getByRole('alert')).toHaveText('The start must not be after the end.')
    expect(queries.length).toBe(1)

    await page.getByLabel('To', {exact: true}).fill('2026-09-12')
    await page.getByRole('button', {name: 'Apply'}).click()
    await expect.poll(() => queries.length).toBe(2)
    expect(queries[1].get('range')).toBe('custom')
    expect(queries[1].get('from')).toBe('2026-09-10')
    expect(queries[1].get('to')).toBe('2026-09-12')
    await settled(page)

    await page.getByRole('button', {name: 'Flows: All flows'}).click()
    const list = page.getByRole('listbox', {name: 'Flows in scope'})
    await expect(list.getByRole('option')).toHaveCount(4)
    await list.getByRole('option', {name: /Invoice sync/}).click()
    await expect.poll(() => queries.length).toBe(3)
    expect(queries[2].get('flows')).toBe('flow-1')
    await page.keyboard.press('Escape')
    await expect(page.getByRole('button', {name: 'Flows: 1 flow'})).toBeFocused()
  })

  test('the section nav scrolls the page, not the shell', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 900})
    await stubAnalytics(page)
    await openAnalytics(page)
    await settled(page)
    await shellStill(page)

    await page.getByRole('button', {name: 'Findings', exact: true}).click()
    const findings = page.getByRole('heading', {name: 'Findings', exact: true, level: 2})
    await expect(findings).toBeInViewport()
    await expect(findings).toBeFocused()
    await shellStill(page)

    await page.getByRole('button', {name: 'Security', exact: true}).click()
    await expect(page.getByRole('heading', {name: 'Security', exact: true, level: 2})).toBeInViewport()
    await shellStill(page)
    // The status bar at the bottom of the shell is still where it belongs.
    const bar = page.getByText('Ready', {exact: true}).first()
    const box = await bar.boundingBox()
    expect(box && box.y).toBeGreaterThan(850)
  })

  test('every chart has a table twin with all of its rows', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    await stubAnalytics(page)
    await openAnalytics(page)
    await settled(page)

    const byRule = card(page, 'Findings by rule')
    await expect(byRule.locator('.recharts-yAxis .recharts-cartesian-axis-tick')).toHaveCount(4)
    await byRule.getByRole('radio', {name: 'Table'}).click()
    await expect(byRule.getByRole('row')).toHaveCount(5)
    await expect(byRule.getByRole('button', {name: 'Download CSV'})).toBeVisible()

    const trend = card(page, 'Health trend')
    await trend.getByRole('radio', {name: 'Table'}).click()
    await expect(trend.getByRole('row')).toHaveCount(31)
    await expect(trend.getByRole('columnheader', {name: 'Previous period'})).toBeVisible()
  })

  test('keeps every card and axis tick inside the pane at any width', async ({page}) => {
    await stubAnalytics(page)
    for (const width of WIDTHS) {
      await page.setViewportSize({width, height: 900})
      if (width === WIDTHS[0]) {
        await openAnalytics(page)
        await settled(page)
      }
      // Recharts re-measures its axes after a resize; poll until it has.
      await expect
        .poll(() => chartLayoutMetrics(page, '.analytics-page'), {message: `${width}px`, timeout: 5_000})
        .toMatchObject({pageOverflow: 0, spilling: 0, clipped: 0})
      const m = await chartLayoutMetrics(page, '.analytics-page')
      expect(m.cards, `${width}px: cards`).toBeGreaterThan(20)
      expect(m.ticks, `${width}px: ticks`).toBeGreaterThan(30)
    }
  })
})
