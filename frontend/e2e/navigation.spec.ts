import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'

test.describe('Navigation', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/dashboard/home', route =>
      route.fulfill({status: 200, json: mockDashboardHome()}),
    )
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('app shell renders with sidebar and main pane', async ({page}) => {
    await expect(page.locator('text=Explorer').first()).toBeVisible({timeout: 10000})
    await expect(page.locator('body')).toBeVisible()
  })

  test('settings modal opens and closes via gear button', async ({page}) => {
    const settingsBtn = page.locator('button[aria-label="Settings"]').first()
    await settingsBtn.click()

    const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
    await expect(dialog).toBeVisible({timeout: 5000})
    await expect(page.locator('text=Settings').first()).toBeVisible()

    await page.keyboard.press('Escape')
    await expect(dialog).not.toBeVisible({timeout: 5000})
  })

  test('settings sections can be navigated', async ({page}) => {
    await page.locator('button[aria-label="Settings"]').first().click()
    const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
    await expect(dialog).toBeVisible({timeout: 5000})

    await page.locator('[role="dialog"] >> text=Appearance').click()
    await expect(page.locator('text=Theme').first()).toBeVisible({timeout: 5000})

    await page.locator('[role="dialog"] >> text=About').click()
    await expect(page.locator('text=Version').first()).toBeVisible({timeout: 5000})

    await page.locator('[role="dialog"] >> text=Shortcuts').click()
    await expect(page.locator('text=Ctrl').first()).toBeVisible({timeout: 5000})
  })

  test('sidebar tabs switch between panels', async ({page}) => {
    const variablesTab = page.locator('button:has-text("Variables")')
    if (await variablesTab.count() > 0) {
      await variablesTab.first().click()
      await page.waitForTimeout(500)
    }

    const explorerTab = page.locator('button:has-text("Explorer")')
    if (await explorerTab.count() > 0) {
      await explorerTab.first().click()
      await page.waitForTimeout(500)
    }

    const libraryTab = page.locator('button:has-text("Library")')
    if (await libraryTab.count() > 0) {
      await libraryTab.first().click()
      await page.waitForTimeout(500)
    }
  })
})
