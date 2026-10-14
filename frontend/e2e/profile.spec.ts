import {test, expect, Page, Route} from '@playwright/test'
import {mockDashboardHome, setupAuthenticatedPage} from './helpers'

const CURRENT_JTI = 'sess-current'

function fakeJwt(payload: Record<string, unknown>): string {
  const b64 = (s: string) => Buffer.from(s).toString('base64url')
  return `${b64('{"alg":"none"}')}.${b64(JSON.stringify(payload))}.sig`
}

const UA = {
  edge: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.0.0',
  chrome:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
  iphone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1',
}

function sessions() {
  const now = Date.now()
  const iso = (t: number) => new Date(t).toISOString()
  const HOUR = 3600e3
  return [
    {
      id: 'sess-old',
      createdAt: iso(now - 72 * HOUR),
      expiresAt: iso(now + 6 * HOUR),
      userAgent: UA.chrome,
      ip: '172.21.0.1',
    },
    {
      id: CURRENT_JTI,
      createdAt: iso(now - 2 * HOUR),
      expiresAt: iso(now + 22 * HOUR),
      userAgent: UA.edge,
      ip: '172.21.0.1',
    },
    ...Array.from({length: 14}, (_, i) => ({
      id: `sess-${i}`,
      createdAt: iso(now - (i + 1) * 5 * HOUR),
      expiresAt: iso(now + (i + 1) * HOUR),
      userAgent: i % 3 === 0 ? UA.iphone : i % 3 === 1 ? undefined : UA.chrome,
      ip: '172.21.0.1',
    })),
  ]
}

// savedSettings collects the settings documents the page persists (the about-you card saves through them).
const savedSettings: Record<string, unknown>[] = []

async function stubProfile(page: Page) {
  await setupAuthenticatedPage(page)
  await page.addInitScript(
    token => {
      localStorage.setItem('auth_refresh_token', token)
    },
    fakeJwt({jti: CURRENT_JTI, exp: 4102444800}),
  )
  const list = sessions()
  await page.route('**/api/**', async (route: Route) => {
    const p = new URL(route.request().url()).pathname
    switch (p) {
      case '/api/auth/refresh':
        return route.fulfill({
          status: 200,
          json: {
            accessToken: 'fake-access-token',
            expiresAt: '2099-01-01T00:00:00Z',
            refreshToken: fakeJwt({jti: CURRENT_JTI, exp: 4102444800}),
          },
        })
      case '/api/auth/sessions':
        return route.fulfill({status: 200, json: list})
      case '/api/auth/sso/info':
        return route.fulfill({status: 200, json: {enabled: false}})
      case '/api/auth/sso/links':
        return route.fulfill({status: 200, json: []})
      case '/api/orgs':
      case '/api/orgs/':
        return route.fulfill({
          status: 200,
          json: [
            {
              id: 'org-1',
              name: 'RPA-org',
              ownerId: 'user-1',
              members: [
                {userId: 'user-1', role: 'admin', joinedAt: ''},
                {userId: 'user-2', role: 'member', joinedAt: ''},
              ],
              createdAt: '',
              updatedAt: '',
            },
          ],
        })
      case '/api/auth/change-password':
      case '/api/auth/logout':
        return route.fulfill({status: 200, json: {status: 'ok'}})
      case '/api/auth/profile/avatar':
        if (route.request().method() === 'DELETE') {
          return route.fulfill({
            status: 200,
            json: {id: 'user-1', email: 'tester@example.com', role: 'admin', displayName: 'Tester', avatarUrl: ''},
          })
        }
        return route.fulfill({
          status: 200,
          json: {
            id: 'user-1',
            email: 'tester@example.com',
            role: 'admin',
            displayName: 'Tester',
            avatarUrl: '/api/users/user-1/avatar',
          },
        })
      case '/api/users/user-1/avatar':
        return route.fulfill({
          status: 200,
          contentType: 'image/png',
          // A real 1×1 PNG: the client downscales through createImageBitmap before uploading, which refuses
          // bytes that are not a decodable image.
          body: Buffer.from(
            'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
            'base64',
          ),
        })
      case '/api/dashboard/home':
      case '/api/analysis/dashboard':
        return route.fulfill({status: 200, json: mockDashboardHome()})
      case '/api/system/settings':
        if (route.request().method() === 'POST') {
          savedSettings.push(route.request().postDataJSON() as Record<string, unknown>)
          return route.fulfill({status: 200, json: {status: 'ok'}})
        }
        return route.fallback()
      default:
        return route.fallback()
    }
  })
}

async function openProfile(page: Page) {
  await page.goto('/')
  await page.getByRole('button', {name: 'User Profile'}).first().click()
  await expect(page.getByRole('heading', {level: 1, name: 'Tester'})).toBeVisible({timeout: 10_000})
  await expect(page.locator('.profile-session').first()).toBeVisible()
}

function layoutMetrics(page: Page) {
  return page.evaluate(() => {
    const root = document.querySelector<HTMLElement>('.profile-page')!
    const scroller = root.parentElement!
    const nav = document.querySelector<HTMLElement>('.profile-nav')!
    const hero = document.querySelector<HTMLElement>('.profile-hero')!
    const actions = hero.querySelector<HTMLElement>('.profile-hero-actions')!
    const field = document.querySelector<HTMLElement>('.profile-field')!
    return {
      paneWidth: scroller.clientWidth,
      overflow: scroller.scrollWidth - scroller.clientWidth,
      navDirection: getComputedStyle(nav).flexDirection,
      navSticky: getComputedStyle(nav).position,
      actionsFullWidth: Math.abs(actions.getBoundingClientRect().width - hero.clientWidth) < 48,
      fieldColumns: getComputedStyle(field).gridTemplateColumns.split(' ').length,
    }
  })
}

test.describe('User Profile', () => {
  test('wide pane: sticky rail nav, this device pinned first, the long list folded', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 900})
    await stubProfile(page)
    await openProfile(page)

    const m = await layoutMetrics(page)
    expect(m.paneWidth).toBeGreaterThanOrEqual(880)
    expect(m.overflow).toBe(0)
    expect(m.navDirection).toBe('column')
    expect(m.navSticky).toBe('sticky')
    expect(m.actionsFullWidth).toBe(false)
    expect(m.fieldColumns).toBe(2)

    const hero = page.locator('.profile-hero')
    await expect(hero.getByText('tester@example.com')).toBeVisible()
    await expect(hero.getByText('admin', {exact: true})).toBeVisible()
    await expect(hero.getByText('1 organisation')).toBeVisible()
    await expect(hero.getByRole('button', {name: 'Sign out', exact: true})).toBeVisible()

    const rows = page.locator('.profile-session')
    await expect(rows).toHaveCount(5)
    await expect(rows.first()).toContainText('Edge on Windows')
    await expect(rows.first()).toContainText('This device')
    await expect(rows.first()).toContainText('Signed in 2 hours ago from 172.21.0.1')
    await expect(rows.first().getByRole('button')).toHaveCount(0)
    await expect(rows.nth(1).getByRole('button', {name: /^Sign out /})).toBeVisible()
    await page.getByRole('button', {name: 'Show all 16 sessions'}).click()
    await expect(rows).toHaveCount(16)

    await expect(page.getByText('RPA-org')).toBeVisible()
    await expect(page.getByText('2 members')).toBeVisible()

    const nav = page.getByRole('navigation', {name: 'Profile sections'})
    await nav.getByRole('button', {name: 'Data & privacy'}).click()
    await expect(nav.getByRole('button', {name: 'Data & privacy'})).toHaveAttribute('aria-current', 'true')
    await expect(page.locator('#profile-data')).toBeInViewport()
    await expect(page.getByRole('heading', {level: 2, name: 'Data & privacy'})).toBeFocused()

    await expect(page.getByRole('button', {name: 'Permanently delete my account'})).toHaveCount(0)
    await page.getByRole('button', {name: 'Delete account…'}).click()
    const del = page.getByRole('button', {name: 'Permanently delete my account'})
    await expect(del).toBeDisabled()
    await page.getByLabel(/To confirm, type your email address/).fill('tester@example.com')
    await expect(del).toBeEnabled()
    await page.getByRole('button', {name: 'Cancel'}).click()
    await expect(del).toHaveCount(0)
  })

  test('narrow panes: chip nav, stacked hero actions, nothing clipped', async ({page}) => {
    await page.setViewportSize({width: 1200, height: 800})
    await stubProfile(page)
    await openProfile(page)

    let m = await layoutMetrics(page)
    expect(m.paneWidth).toBeLessThan(880)
    expect(m.paneWidth).toBeGreaterThanOrEqual(560)
    expect(m.overflow).toBe(0)
    expect(m.navDirection).toBe('row')
    expect(m.navSticky).toBe('sticky')
    expect(m.actionsFullWidth).toBe(false)
    expect(m.fieldColumns).toBe(2)

    await page.setViewportSize({width: 1000, height: 800})
    await page.waitForTimeout(200)
    m = await layoutMetrics(page)
    expect(m.paneWidth).toBeLessThan(560)
    expect(m.overflow).toBe(0)
    expect(m.navDirection).toBe('row')
    expect(m.actionsFullWidth).toBe(true)
    expect(m.fieldColumns).toBe(1)

    await page.setViewportSize({width: 390, height: 800})
    await page.waitForTimeout(200)
    m = await layoutMetrics(page)
    expect(m.paneWidth).toBeLessThanOrEqual(390)
    expect(m.overflow).toBe(0)
    expect(m.actionsFullWidth).toBe(true)
    await expect(page.getByRole('button', {name: 'Sign out other sessions'})).toBeVisible()
  })

  test('changing the password shows the rules, then signs out with an explanation', async ({page}) => {
    await page.setViewportSize({width: 1400, height: 900})
    await stubProfile(page)
    await openProfile(page)

    const submit = page.getByRole('button', {name: 'Update password'})
    await expect(submit).toBeDisabled()
    await page.getByLabel('Current password').fill('old-password')
    await page.getByLabel('New password', {exact: true}).fill('correct-horse-battery')
    await expect(page.locator('#change-password-rules li[data-met]')).toHaveCount(1)
    await page.getByLabel('New password', {exact: true}).fill('Correct-horse-1')
    await expect(page.locator('#change-password-rules li[data-met]')).toHaveCount(2)
    await expect(submit).toBeDisabled()
    await page.getByLabel('Confirm new password').fill('Correct-horse-1')
    await expect(submit).toBeEnabled()

    await submit.click()
    await expect(page.getByRole('button', {name: /^Sign in$/})).toBeVisible({timeout: 10_000})
    await expect(page.getByRole('status')).toHaveText('Password updated. Sign in again with your new password.')
    const stored = await page.evaluate(
      () => localStorage.getItem('auth_refresh_token') ?? sessionStorage.getItem('auth_refresh_token'),
    )
    expect(stored).toBeNull()
  })

  test('uploads a picture, removes it, and saves the about-you card', async ({page}) => {
    await page.setViewportSize({width: 1400, height: 900})
    savedSettings.length = 0
    await stubProfile(page)
    await openProfile(page)

    // The about-you card is on the page with honest empty values.
    const about = page.locator('#profile-details')
    await expect(about.getByRole('heading', {level: 3, name: 'About you'})).toBeVisible()
    await expect(about.getByText('Not set').first()).toBeVisible()

    // Upload: the serving route takes over the profile, and Remove appears.
    await page.getByRole('button', {name: 'Edit profile'}).click()
    await page.locator('input[type=file]').setInputFiles({
      name: 'me.png',
      mimeType: 'image/png',
      buffer: Buffer.from(
        'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
        'base64',
      ),
    })
    await expect(page.getByRole('button', {name: 'Remove picture'})).toBeVisible({timeout: 10_000})
    await expect(page.getByLabel('Avatar URL')).toHaveValue('/api/users/user-1/avatar')

    // Removal clears it again.
    await page.getByRole('button', {name: 'Remove picture'}).click()
    await expect(page.getByLabel('Avatar URL')).toHaveValue('')
    await expect(page.getByRole('button', {name: 'Remove picture'})).toHaveCount(0)

    // The about-you card saves through the settings document's profile section.
    await about.getByRole('button', {name: 'Edit'}).click()
    await about.getByLabel('Bio').fill('Automating the boring parts.')
    await about.getByLabel('Job title').fill('RPA Developer')
    await about.getByLabel('Timezone').selectOption('Europe/Belgrade')
    await about.getByRole('button', {name: 'Save changes'}).click()
    await expect(about.getByText('Automating the boring parts.')).toBeVisible({timeout: 10_000})
    await expect
      .poll(async () => {
        const last = savedSettings[savedSettings.length - 1]
        return (last?.profile as Record<string, string>) ?? null
      })
      .toEqual({bio: 'Automating the boring parts.', jobTitle: 'RPA Developer', timezone: 'Europe/Belgrade'})
  })
})
