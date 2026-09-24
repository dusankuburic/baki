import {test, expect, Page, Route} from '@playwright/test'
import {mockDashboardHome, setupAuthenticatedPage} from './helpers'

/**
 * Cloud Library layout E2E. The library lives in the main pane, whose width
 * the sidebar and inspector set, so the grid, the list and the detail panel
 * must answer to that width: cards used to shrink to ~105px with their
 * counts spilling out of the box, and the list's fixed pixel columns crushed
 * the name column to nothing while its heading still showed.
 */

function flows() {
  const names = [
    ['Customer Data Sync (sample)', 10, 1],
    ['padframework', 116, 13],
    ['samples', 10114, 51],
    ['browsercloser', 141, 4],
    ['Variables', 75, 5],
  ] as const
  return Array.from({length: 16}, (_, i) => {
    const [name, blockCount, subflowCount] = names[i % names.length]
    return {
      id: `f${i}`,
      name,
      ownerId: 'user-1',
      ownerDisplayName: 'dusan.kuburic@outlook.com',
      organizationId: '',
      blockCount,
      subflowCount,
      updatedAt: new Date(Date.now() - (i + 1) * 3 * 86400e3).toISOString(),
      version: 0,
      isSharedWithMe: false,
      canEdit: true,
      canDelete: true,
      canShare: true,
      tags: [],
    }
  })
}

async function stubLibrary(page: Page) {
  await setupAuthenticatedPage(page)
  const items = flows()
  await page.route('**/api/**', async (route: Route) => {
    const p = new URL(route.request().url()).pathname
    if (p === '/api/library') {
      await route.fulfill({status: 200, json: {items, total: items.length, offset: 0, limit: 24}})
      return
    }
    const m = /^\/api\/library\/([^/]+)(\/versions)?$/.exec(p)
    if (m) {
      const f = items.find(x => x.id === m[1])
      if (m[2]) await route.fulfill({status: 200, json: []})
      else await route.fulfill({status: 200, json: {...f, healthScore: 0, errorCount: 10, warningCount: 18}})
      return
    }
    if (p === '/api/analysis/dashboard') {
      await route.fulfill({status: 200, json: mockDashboardHome()})
      return
    }
    await route.fallback()
  })
}

async function openLibrary(page: Page) {
  await page.goto('/')
  await page.getByRole('button', {name: 'Cloud Library'}).first().click()
  await expect(page.getByText('padframework', {exact: true}).first()).toBeVisible({timeout: 10_000})
}

// Card boxes and their footers must contain their text at any width.
function cardOverflow(page: Page) {
  return page.evaluate(() => {
    const cards = Array.from(document.querySelectorAll<HTMLElement>('.library-grid > button'))
    const spilling = cards.filter(c => {
      const meta = c.querySelector<HTMLElement>(':scope > div:nth-child(2)')
      return c.scrollWidth > c.clientWidth + 1 || (meta ? meta.scrollWidth > meta.clientWidth + 1 : false)
    })
    return {cards: cards.length, spilling: spilling.length, minWidth: Math.min(...cards.map(c => c.clientWidth))}
  })
}

function listMetrics(page: Page) {
  return page.evaluate(() => {
    const list = document.querySelector<HTMLElement>('.library-list')!
    const header = list.querySelector<HTMLElement>('.library-row')!
    const nameCell = list.querySelector<HTMLElement>('li .library-row > div, li.library-row > div')!
    return {
      headerClip: header.scrollWidth - header.clientWidth,
      nameWidth: nameCell.getBoundingClientRect().width,
      ownerShown: getComputedStyle(header.querySelector('.library-col-owner')!).display !== 'none',
      subflowsShown: getComputedStyle(header.querySelector('.library-col-subflows')!).display !== 'none',
    }
  })
}

test.describe('Cloud Library layout', () => {
  test('narrow main pane: cards keep their content, the list keeps its name column, details slide over', async ({
    page,
  }) => {
    await page.setViewportSize({width: 1200, height: 900})
    await stubLibrary(page)
    await openLibrary(page)

    const grid = await cardOverflow(page)
    expect(grid.cards).toBe(16)
    expect(grid.spilling).toBe(0)
    expect(grid.minWidth).toBeGreaterThanOrEqual(240)

    // No room to dock the detail panel: it is hidden until a flow is
    // selected, then slides over the results with its actions reachable.
    const details = page.getByRole('complementary', {name: 'Flow details'})
    await expect(details).toBeHidden()
    await page.locator('.library-grid > button').filter({hasText: 'padframework'}).first().click()
    await expect(details).toBeVisible()
    await expect(details.getByRole('button', {name: 'Load flow'})).toBeVisible()
    await details.getByRole('button', {name: 'Close detail panel'}).click()
    await expect(details).toBeHidden()

    await page.getByLabel('List view').click()
    await expect(page.getByText('Updated', {exact: true})).toBeVisible()
    const list = await listMetrics(page)
    expect(list.headerClip).toBe(0)
    expect(list.nameWidth).toBeGreaterThanOrEqual(120)
    // The lowest-ranked column went first; its heading went with it.
    expect(list.ownerShown).toBe(false)
    await expect(page.getByText('Owner', {exact: true})).toBeHidden()
  })

  test('wide main pane: the detail panel docks and every list column shows', async ({page}) => {
    await page.setViewportSize({width: 1800, height: 900})
    await stubLibrary(page)
    await openLibrary(page)

    // The same section switcher as every other system view, not a private
    // header with a back link.
    const sections = page.getByRole('radiogroup', {name: 'Workspace'})
    await expect(sections.getByRole('radio', {name: 'Library'})).toBeChecked()
    await expect(page.locator('.main-toolbar').getByText('Cloud Library')).toBeVisible()
    await expect(page.getByRole('button', {name: 'Home', exact: true})).toHaveCount(0)
    await expect(page.getByText('16 flows')).toBeVisible()
    await sections.getByRole('radio', {name: 'Profile'}).click()
    await expect(page.getByRole('heading', {level: 1, name: 'Tester'})).toBeVisible()
    await sections.getByRole('radio', {name: 'Library'}).click()

    await expect(page.getByText('Select a flow to see its details.')).toBeVisible()
    const grid = await cardOverflow(page)
    expect(grid.spilling).toBe(0)
    expect(grid.minWidth).toBeGreaterThanOrEqual(240)

    await page.getByLabel('List view').click()
    await expect(page.getByText('Owner', {exact: true})).toBeVisible()
    const list = await listMetrics(page)
    expect(list.headerClip).toBe(0)
    expect(list.ownerShown).toBe(true)
    expect(list.subflowsShown).toBe(true)
  })
})
