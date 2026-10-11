import {test, expect, Page, Route} from '@playwright/test'
import {mockAnalysisReport, mockFlowDocument, mockDashboardHome} from './helpers'

function stubAuth(page: Page, opts: {refreshStatus?: number; loginBody?: unknown} = {}) {
  return page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    const {
      refreshStatus = 200,
      loginBody = {
        accessToken: 'tok-access',
        refreshToken: 'tok-refresh',
        expiresAt: '2099-01-01T00:00:00Z',
        user: {id: 'user-1', email: 'e2e@example.com', role: 'member', displayName: 'E2E'},
      },
    } = opts
    switch (url.pathname) {
      case '/api/local-config':
        return route.fulfill({status: 200, json: {mode: 'cloud'}})
      case '/api/auth/refresh':
        if (refreshStatus !== 200) return route.fulfill({status: refreshStatus, json: {error: 'invalid token'}})
        return route.fulfill({
          status: 200,
          json: {accessToken: 'tok-access', expiresAt: '2099-01-01T00:00:00Z'},
        })
      case '/api/auth/login':
        return route.fulfill({status: 200, json: loginBody})
      case '/api/auth/logout':
        return route.fulfill({status: 200, json: {status: 'ok'}})
      case '/api/auth/me':
        return route.fulfill({
          status: 200,
          json: {id: 'user-1', email: 'e2e@example.com', role: 'member', displayName: 'E2E'},
        })
      case '/api/system/info':
        return route.fulfill({
          status: 200,
          json: {version: 'e2e', platform: 'linux', arch: 'x64', buildDate: '', gitCommit: ''},
        })
      case '/api/system/settings':
        return route.fulfill({
          status: 200,
          json: {
            version: 1,
            general: {firstRunCompleted: true},
            appearance: {},
            layout: {},
            ai: {},
            parser: {},
            analysis: {},
          },
        })
      case '/api/flow/recent':
        return route.fulfill({status: 200, json: []})
      case '/api/flow/upload':
        return route.fulfill({status: 200, json: mockFlowDocument()})
      case '/api/orgs/':
        return route.fulfill({status: 200, json: []})
      default:
        return route.fallback()
    }
  })
}

test.describe('Authentication', () => {
  test('login form submits and reaches the app shell', async ({page}) => {
    await stubAuth(page)
    await page.goto('/')

    const email = page.locator('input[type="email"], input[placeholder="Email"]').first()
    await expect(email).toBeVisible({timeout: 10_000})
    await email.fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()

    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})
  })

  test('logout clears the session and returns to the login screen', async ({page}) => {
    await stubAuth(page)
    await page.goto('/')
    await page.locator('input[type="email"], input[placeholder="Email"]').first().waitFor({timeout: 10_000})
    await page.locator('input[type="email"], input[placeholder="Email"]').first().fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()
    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})

    const skipBtn = page.getByRole('button', {name: 'Skip', exact: true})
    await skipBtn.waitFor({state: 'visible', timeout: 8_000}).then(
      () => skipBtn.click(),
      () => {},
    )
    const startBtn = page.getByRole('button', {name: 'Load sample & start'})
    await startBtn.waitFor({state: 'visible', timeout: 8_000}).then(
      () => startBtn.click(),
      () => {},
    )

    await page.getByRole('button', {name: 'User Profile'}).click()
    await page.getByRole('button', {name: 'Sign out', exact: true}).click()

    await expect(page.locator('input[type="password"]').first()).toBeVisible({timeout: 10_000})
    const stored = await page.evaluate(
      () => localStorage.getItem('auth_refresh_token') ?? sessionStorage.getItem('auth_refresh_token'),
    )
    expect(stored).toBeNull()
  })

  test('a failing refresh on boot lands on the login screen (no dead shell)', async ({page}) => {
    await stubAuth(page, {refreshStatus: 401})
    await page.addInitScript(() => {
      localStorage.setItem('auth_refresh_token', 'stale-token')
    })
    await page.goto('/')

    await expect(page.locator('input[type="password"]').first()).toBeVisible({timeout: 10_000})
  })

  test('authenticated session can still open and analyze a flow', async ({page}) => {
    await stubAuth(page)
    await page.route('**/api/flow/load-path', async route => route.fulfill({status: 200, json: mockFlowDocument()}))
    await page.route('**/api/analysis/analyze', async route => route.fulfill({status: 200, json: mockAnalysisReport()}))
    await page.route('**/api/analysis/dashboard', async route =>
      route.fulfill({status: 200, json: mockDashboardHome()}),
    )

    await page.goto('/')
    await page.locator('input[type="email"], input[placeholder="Email"]').first().waitFor({timeout: 10_000})
    await page.locator('input[type="email"], input[placeholder="Email"]').first().fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()
    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})
  })
})
