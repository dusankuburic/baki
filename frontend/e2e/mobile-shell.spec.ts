import {test, expect} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'

// Mobile smoke (U5b): the app is "desktop shrunk with drawers" below md —
// this spec pins the contract that the shell, drawers, and command palette
// stay USABLE at 375px. Run: npm run e2e -- mobile-shell.spec.ts
test.describe('Mobile shell @375px', () => {
  test.beforeEach(async ({page}) => {
    await page.setViewportSize({width: 375, height: 667})
    await setupAuthenticatedPage(page)
    await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('hamburger opens the sidebar drawer and it closes by swipe', async ({page}) => {
    const hamburger = page.locator('button[aria-label*="menu" i], button[aria-label*="sidebar" i]').first()
    await expect(hamburger).toBeVisible({timeout: 10_000})
    await hamburger.click()

    const drawer = page.locator('[role="dialog"][aria-label*="Sidebar" i]').first()
    await expect(drawer).toBeVisible({timeout: 5000})
    await expect(page.locator('text=Explorer').first()).toBeVisible()

    // Swipe the LEFT drawer right-to-left to close (pointer events).
    const box = await drawer.boundingBox()
    if (box) {
      await page.mouse.move(box.x + box.width - 10, box.y + 100)
      await page.mouse.down()
      // Two-step drag keeps Chrome from treating it as a scroll.
      await page.mouse.move(box.x + box.width - 40, box.y + 100, {steps: 4})
      await page.mouse.move(box.x - 10, box.y + 100, {steps: 8})
      await page.mouse.up()
    }
    await expect(drawer).not.toBeVisible({timeout: 5000})
  })

  test('command palette renders full-screen width', async ({page}) => {
    await page.keyboard.press('ControlOrMeta+k')
    const palette = page.locator('[role="dialog"], [role="listbox"]').first()
    await expect(palette).toBeVisible({timeout: 5000})
    const box = await palette.boundingBox()
    expect(box).toBeTruthy()
    // Full-bleed below md: within a few px of the viewport width.
    if (box) expect(Math.abs(box.width - 375)).toBeLessThanOrEqual(8)
    await page.keyboard.press('Escape')
  })

  test('no horizontal overflow at 375px', async ({page}) => {
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(1)
  })
})
