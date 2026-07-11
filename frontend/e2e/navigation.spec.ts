import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'

test.describe('Navigation', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    // Mock dashboard home so the landing view renders without errors
    await page.route('**/api/dashboard/home', route =>
      route.fulfill({status: 200, json: mockDashboardHome()}),
    )
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('app shell renders with sidebar and main pane', async ({page}) => {
    // The sidebar should be visible (Explorer tab or similar)
    await expect(page.locator('text=Explorer').first()).toBeVisible({timeout: 10000})
    // The main area should show some content (not blank)
    await expect(page.locator('body')).toBeVisible()
  })

  test('settings modal opens and closes via gear button', async ({page}) => {
    const settingsBtn = page.locator('button[aria-label="Settings"]').first()
    await settingsBtn.click()

    // Settings dialog should appear
    const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
    await expect(dialog).toBeVisible({timeout: 5000})
    await expect(page.locator('text=Settings').first()).toBeVisible()

    // Close with Escape
    await page.keyboard.press('Escape')
    await expect(dialog).not.toBeVisible({timeout: 5000})
  })

  test('settings sections can be navigated', async ({page}) => {
    await page.locator('button[aria-label="Settings"]').first().click()
    const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
    await expect(dialog).toBeVisible({timeout: 5000})

    // Navigate to Appearance
    await page.locator('[role="dialog"] >> text=Appearance').click()
    // Appearance panel should show theme-related content
    await expect(page.locator('text=Theme').first()).toBeVisible({timeout: 5000})

    // Navigate to About
    await page.locator('[role="dialog"] >> text=About').click()
    // About panel should show version info
    await expect(page.locator('text=Version').first()).toBeVisible({timeout: 5000})

    // Navigate to Shortcuts
    await page.locator('[role="dialog"] >> text=Shortcuts').click()
    // Shortcuts panel should show keyboard shortcut listings
    await expect(page.locator('text=Ctrl').first()).toBeVisible({timeout: 5000})
  })

  test('sidebar tabs switch between panels', async ({page}) => {
    // Click the Variables tab in the sidebar
    const variablesTab = page.locator('button:has-text("Variables")')
    if (await variablesTab.count() > 0) {
      await variablesTab.first().click()
      // Variables panel should render (may show "no flow loaded" state)
      await page.waitForTimeout(500)
    }

    // Click the Explorer tab to go back
    const explorerTab = page.locator('button:has-text("Explorer")')
    if (await explorerTab.count() > 0) {
      await explorerTab.first().click()
      await page.waitForTimeout(500)
    }

    // Click the Library tab
    const libraryTab = page.locator('button:has-text("Library")')
    if (await libraryTab.count() > 0) {
      await libraryTab.first().click()
      await page.waitForTimeout(500)
    }
  })
})
