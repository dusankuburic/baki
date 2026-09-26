import {test, expect, type Page} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'

// The Settings dialog's frame, sidebar and keyboard contract. navigation.spec
// keeps the older text-selector checks; this file pins the new shell.

async function openSettings(page: Page) {
  await page.locator('button[aria-label="Settings"]').first().click()
  const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
  await expect(dialog).toBeVisible({timeout: 5000})
  // The dialog enters with a scale(0.96) animation; measure it at rest.
  await dialog.evaluate(el => Promise.all(el.getAnimations().map(a => a.finished)))
  return dialog
}

test.describe('Settings modal', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('fills a 1200×960 frame on a large display, with a 240px section rail', async ({page}) => {
    await page.setViewportSize({width: 1920, height: 1080})
    const dialog = await openSettings(page)
    const box = await dialog.boundingBox()
    expect(box).toBeTruthy()
    expect(Math.abs(box!.width - 1200)).toBeLessThanOrEqual(2)
    expect(Math.abs(box!.height - 960)).toBeLessThanOrEqual(2)
    const rail = await dialog.locator('.settings-nav').boundingBox()
    expect(Math.abs(rail!.width - 240)).toBeLessThanOrEqual(2)

    // One tablist holds the groups and the pinned About entry; the stub runs
    // in cloud mode, so the Workspace group is present.
    const tabs = dialog.getByRole('tablist', {name: 'Settings sections'})
    await expect(tabs).toHaveAttribute('aria-orientation', 'vertical')
    await expect(dialog.getByRole('tab', {name: 'General'})).toHaveAttribute('aria-selected', 'true')
    await expect(dialog.getByRole('tab', {name: 'Organizations'})).toBeVisible()
    await expect(dialog.getByRole('tab', {name: 'About'})).toBeVisible()
    await expect(dialog.getByRole('heading', {level: 2, name: 'General'})).toBeVisible()
  })

  test('shrinks to the viewport on a small laptop', async ({page}) => {
    await page.setViewportSize({width: 1366, height: 768})
    const dialog = await openSettings(page)
    const box = await dialog.boundingBox()
    expect(box).toBeTruthy()
    expect(Math.abs(box!.width - 1200)).toBeLessThanOrEqual(2)
    expect(Math.abs(box!.height - 720)).toBeLessThanOrEqual(2)
  })

  test('switching sections resets the scroll, and arrow keys move the selection', async ({page}) => {
    // Short enough that the Shortcuts panel has to scroll.
    await page.setViewportSize({width: 1366, height: 768})
    const dialog = await openSettings(page)
    await dialog.getByRole('tab', {name: 'Shortcuts'}).click()
    await expect(dialog.getByRole('heading', {level: 2, name: 'Shortcuts'})).toBeVisible()
    await expect(page.locator('text=Ctrl').first()).toBeVisible()

    const body = dialog.locator('#settings-panel')
    await body.evaluate(el => {
      el.scrollTop = 400
    })
    expect(await body.evaluate(el => el.scrollTop)).toBeGreaterThan(0)

    await dialog.getByRole('tab', {name: 'Appearance'}).click()
    await expect(dialog.getByRole('heading', {level: 2, name: 'Appearance'})).toBeVisible()
    expect(await body.evaluate(el => el.scrollTop)).toBe(0)

    await dialog.getByRole('tab', {name: 'Appearance'}).focus()
    await page.keyboard.press('ArrowDown')
    await expect(dialog.getByRole('tab', {name: 'Shortcuts'})).toHaveAttribute('aria-selected', 'true')
    await expect(dialog.getByRole('heading', {level: 2, name: 'Shortcuts'})).toBeVisible()
  })

  test('search filters the sections, Escape clears before it closes, and the last section is remembered', async ({
    page,
  }) => {
    await page.setViewportSize({width: 1920, height: 1080})
    const dialog = await openSettings(page)
    const search = dialog.getByRole('textbox', {name: 'Search settings'})

    await search.fill('theme')
    await expect(dialog.getByRole('tab')).toHaveCount(1)
    await search.press('Enter')
    await expect(dialog.getByRole('tab', {name: 'Appearance'})).toHaveAttribute('aria-selected', 'true')
    await expect(page.locator('text=Color Theme').first()).toBeVisible()

    await search.press('Escape')
    await expect(search).toHaveValue('')
    await expect(dialog).toBeVisible()
    await expect(dialog.getByRole('tab', {name: 'General'})).toBeVisible()

    await search.press('Escape')
    await expect(dialog).not.toBeVisible({timeout: 5000})

    const again = await openSettings(page)
    await expect(again.getByRole('tab', {name: 'Appearance'})).toHaveAttribute('aria-selected', 'true')
  })
})
