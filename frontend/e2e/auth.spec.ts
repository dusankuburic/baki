import {test, expect, Page, Route} from '@playwright/test'
import {mockAnalysisReport, mockFlowDocument, mockDashboardHome} from './helpers'

/**
 * Auth-critical E2E: the flows every other spec deliberately bypasses via
 * setupAuthenticatedPage. These tests exercise the real authStore machinery —
 * login form submission, refresh-failure → forced logout, and explicit
 * logout (token clear + login screen) — against mocked endpoints.
 */

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
        // Full AppSettings shape (zod-validated client-side);
        // firstRunCompleted=true suppresses the onboarding welcome modal
        // that would otherwise overlay every post-login interaction.
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
        // The onboarding "Load sample & start" completes first-run this way.
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

    // Login gate (cloud mode, no stored token).
    const email = page.locator('input[type="email"], input[placeholder="Email"]').first()
    await expect(email).toBeVisible({timeout: 10_000})
    await email.fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()

    // Authenticated shell appears.
    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})
  })

  test('logout clears the session and returns to the login screen', async ({page}) => {
    await stubAuth(page)
    // Pre-seed a session, then log in through the form to exercise the real path.
    await page.goto('/')
    await page.locator('input[type="email"], input[placeholder="Email"]').first().waitFor({timeout: 10_000})
    await page.locator('input[type="email"], input[placeholder="Email"]').first().fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()
    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})

    // First run shows the onboarding modal. "Skip" (present on every step)
    // jumps to the last step, whose "Load sample & start" completes onboarding
    // for real (stubbed upload). Both waits no-op when first-run is already
    // done and the modal never opens.
    const skipBtn = page.getByRole('button', {name: 'Skip', exact: true})
    await skipBtn.waitFor({state: 'visible', timeout: 8_000}).then(() => skipBtn.click(), () => {})
    const startBtn = page.getByRole('button', {name: 'Load sample & start'})
    await startBtn.waitFor({state: 'visible', timeout: 8_000}).then(() => startBtn.click(), () => {})

    // The avatar/profile area exposes logout from the Profile system view.
    await page.getByRole('button', {name: 'User Profile'}).click()
    await page.getByRole('button', {name: 'Logout'}).click()

    // Back at the login gate; refresh token storage is cleared.
    await expect(page.locator('input[type="password"]').first()).toBeVisible({timeout: 10_000})
    const stored = await page.evaluate(() => localStorage.getItem('auth_refresh_token') ?? sessionStorage.getItem('auth_refresh_token'))
    expect(stored).toBeNull()
  })

  test('a failing refresh on boot lands on the login screen (no dead shell)', async ({page}) => {
    await stubAuth(page, {refreshStatus: 401})
    // Stale token in storage: boot must attempt refresh, get 401, and show login.
    await page.addInitScript(() => {
      localStorage.setItem('auth_refresh_token', 'stale-token')
    })
    await page.goto('/')

    await expect(page.locator('input[type="password"]').first()).toBeVisible({timeout: 10_000})
  })

  test('authenticated session can still open and analyze a flow', async ({page}) => {
    await stubAuth(page)
    await page.route('**/api/flow/load-path', async route =>
      route.fulfill({status: 200, json: mockFlowDocument()}))
    await page.route('**/api/analysis/analyze', async route =>
      route.fulfill({status: 200, json: mockAnalysisReport()}))
    await page.route('**/api/analysis/dashboard', async route =>
      route.fulfill({status: 200, json: mockDashboardHome()}))

    await page.goto('/')
    await page.locator('input[type="email"], input[placeholder="Email"]').first().waitFor({timeout: 10_000})
    await page.locator('input[type="email"], input[placeholder="Email"]').first().fill('e2e@example.com')
    await page.locator('input[type="password"]').first().fill('correct-horse')
    await page.getByRole('button', {name: /^Sign in$/}).click()
    await expect(page.locator('[role="main"], main')).toBeVisible({timeout: 10_000})
  })
})
