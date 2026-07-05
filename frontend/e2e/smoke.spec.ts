import {test, expect} from '@playwright/test'

// Smoke test: verifies the app loads and shows the initial UI. This is the
// minimal E2E scaffolding — add tests for the core flows (load flow, analyze,
// filter findings, apply fix, AI chat) incrementally.

test.describe('App smoke', () => {
  test('loads without crash', async ({page}) => {
    await page.goto('/')
    // The app should render some visible content (not a blank page)
    await expect(page.locator('body')).toBeVisible()
    // Check that we don't have a crash error overlay
    await expect(page.locator('text=Application error')).not.toBeVisible({timeout: 5000})
  })

  test('shows login or app shell', async ({page}) => {
    await page.goto('/')
    // In cloud mode: shows login form. In desktop mode: shows app shell.
    // Either way, the page should not be blank.
    await page.waitForLoadState('networkidle')
    const hasLogin = await page.locator('input[type="password"], input[name="password"]').count()
    const hasContent = await page.locator('main, [role="main"], .app-shell').count()
    expect(hasLogin + hasContent).toBeGreaterThan(0)
  })
})

test.describe('Shared report viewer', () => {
  test('shows error for missing token', async ({page}) => {
    await page.goto('/shared')
    await expect(page.locator('text=No share token')).toBeVisible({timeout: 5000})
  })

  test('shows error for invalid token', async ({page}) => {
    await page.goto('/shared?token=invalid')
    await expect(page.locator('text=Cannot open report')).toBeVisible({timeout: 5000})
  })
})
