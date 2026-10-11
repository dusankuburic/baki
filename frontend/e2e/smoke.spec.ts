import {test, expect} from '@playwright/test'

test.describe('App smoke', () => {
  test('loads without crash', async ({page}) => {
    await page.goto('/')
    await expect(page.locator('body')).toBeVisible()
    await expect(page.locator('text=Application error')).not.toBeVisible({timeout: 5000})
  })

  test('shows login or app shell', async ({page}) => {
    await page.goto('/')
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
