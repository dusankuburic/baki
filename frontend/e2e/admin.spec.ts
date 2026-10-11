import {test, expect, Page, Route} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'
import {WIDTHS} from './chartChecks'

type Recorded = {path: string; params: URLSearchParams; method: string; body: unknown}

const USERS = Array.from({length: 17}, (_, i) => {
  const n = i + 1
  return {
    id: n === 1 ? 'user-1' : `u${n}`,
    email: n === 1 ? 'tester@example.com' : n === 2 ? 'bob@example.com' : `user${n}@example.com`,
    role: n === 1 ? 'admin' : n === 3 ? 'viewer' : n === 17 ? '' : 'member',
    emailVerified: n % 2 === 0,
    displayName: n === 2 ? 'Bob' : n === 5 ? 'Alice' : '',
    avatarUrl: '',
    createdAt: `2026-09-${String(30 - n).padStart(2, '0')}T10:00:00Z`,
    updatedAt: `2026-09-${String(30 - n).padStart(2, '0')}T10:00:00Z`,
  }
})

const STATS = {total: 17, byRole: {admin: 1, member: 14, viewer: 1, guest: 0, none: 1}, unverified: 9, locked: 0}
const ACTIONS = [
  'auth.login',
  'auth.login_failure',
  'admin.role_change',
  'settings.change',
  'flow.upload',
  'org_channel_save',
]

const EVENTS = Array.from({length: 40}, (_, i) => {
  const failure = i % 5 === 0
  return {
    id: `e${i}`,
    userId: failure ? '' : 'user-1',
    email: failure ? '' : 'tester@example.com',
    action: failure ? 'auth.login_failure' : 'settings.change',
    resourceType: failure ? 'user' : 'settings',
    resourceId: failure ? 'user-1' : 'ab1f6fc3',
    ip: '172.21.0.1',
    meta: failure ? {reason: 'invalid_password'} : {scope: 'user'},
    createdAt: new Date(Date.UTC(2026, 8, 25, 21, 59 - i)).toISOString(),
  }
})

function int(params: URLSearchParams, name: string, def: number) {
  const v = params.get(name)
  return v === null ? def : Number(v)
}

async function stubAdmin(page: Page) {
  await setupAuthenticatedPage(page)
  const calls: Recorded[] = []
  await page.route('**/api/admin/**', async (route: Route) => {
    const req = route.request()
    const url = new URL(req.url())
    calls.push({path: url.pathname, params: url.searchParams, method: req.method(), body: req.postDataJSON()})
    switch (url.pathname) {
      case '/api/admin/users/list': {
        const limit = int(url.searchParams, 'limit', 50)
        const offset = int(url.searchParams, 'offset', 0)
        const q = (url.searchParams.get('q') ?? '').toLowerCase()
        const role = url.searchParams.get('role') ?? ''
        let rows = USERS
        if (q) rows = rows.filter(u => u.email.includes(q) || u.displayName.toLowerCase().includes(q))
        if (role) rows = rows.filter(u => (role === 'none' ? u.role === '' : u.role === role))
        return route.fulfill({
          status: 200,
          json: {items: rows.slice(offset, offset + limit), total: rows.length, offset, limit},
        })
      }
      case '/api/admin/users/stats':
        return route.fulfill({status: 200, json: STATS})
      case '/api/admin/migration/status':
        return route.fulfill({status: 200, json: {configured: false, running: false}})
      case '/api/admin/audit/actions':
        return route.fulfill({status: 200, json: ACTIONS})
      case '/api/admin/audit': {
        const limit = int(url.searchParams, 'limit', 100)
        const offset = int(url.searchParams, 'offset', 0)
        const action = url.searchParams.get('action')
        const userId = url.searchParams.get('userId')
        let rows = EVENTS
        if (action) rows = rows.filter(e => e.action === action)
        if (userId)
          rows = rows.filter(e => e.userId === userId || (e.resourceType === 'user' && e.resourceId === userId))
        return route.fulfill({status: 200, json: rows.slice(offset, offset + limit)})
      }
      case '/api/admin/system/health':
        return route.fulfill({
          status: 200,
          json: {
            database: {status: 'ok'},
            blob: {status: 'skipped'},
            redis: {status: 'error', error: 'connection refused'},
            overall: 'degraded',
          },
        })
      default:
        if (url.pathname.startsWith('/api/admin/users/') && url.pathname.endsWith('/role')) {
          return route.fulfill({status: 200, json: {status: 'ok'}})
        }
        return route.fallback()
    }
  })
  await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  await page.route('**/api/analysis/dashboard', route => route.fulfill({status: 200, json: mockDashboardHome()}))
  return calls
}

async function openAdmin(page: Page) {
  await page.goto('/')
  await page.getByRole('button', {name: 'Admin Dashboard'}).first().click()
  await expect(page.getByRole('heading', {level: 1, name: 'Admin Dashboard'})).toBeVisible({timeout: 10_000})
  await expect(page.getByText('bob@example.com')).toBeVisible()
}

function last(calls: Recorded[], path: string): Recorded {
  const hit = calls.filter(c => c.path === path).at(-1)
  if (!hit) throw new Error(`no request to ${path}`)
  return hit
}

async function shellStill(page: Page) {
  const doc = await page.evaluate(() => ({
    top: document.documentElement.scrollTop,
    height: document.documentElement.scrollHeight,
    viewport: window.innerHeight,
  }))
  expect(doc.top).toBe(0)
  expect(doc.height).toBe(doc.viewport)
}

test.describe('Admin Dashboard', () => {
  test('pages users ten at a time and reports the range', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    const calls = await stubAdmin(page)
    await openAdmin(page)

    const first = calls.find(c => c.path === '/api/admin/users/list')!
    expect(first.params.get('limit')).toBe('10')
    expect(first.params.get('offset')).toBe('0')

    const nav = page.getByRole('navigation', {name: 'Users pagination'})
    await expect(nav).toContainText('Showing 1–10 of 17')
    await nav.getByRole('button', {name: 'Next page'}).click()
    await expect(nav).toContainText('Showing 11–17 of 17')
    await expect(page.getByText('user17@example.com')).toBeVisible()
    await expect(nav.getByRole('button', {name: 'Next page'})).toBeDisabled()
    expect(last(calls, '/api/admin/users/list').params.get('offset')).toBe('10')

    await nav.getByRole('combobox', {name: 'Rows per page'}).selectOption('25')
    await expect(nav).toContainText('Showing 1–17 of 17')
    const bigger = last(calls, '/api/admin/users/list')
    expect(bigger.params.get('limit')).toBe('25')
    expect(bigger.params.get('offset')).toBe('0')
  })

  test('searches, filters by role and changes a role after one confirmation', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    const calls = await stubAdmin(page)
    await openAdmin(page)

    await page.getByRole('textbox', {name: 'Search by email or name'}).fill('ali')
    await expect(page.getByRole('navigation', {name: 'Users pagination'})).toContainText('Showing 1–1 of 1')
    await expect(page.getByText('user5@example.com')).toBeVisible()
    expect(last(calls, '/api/admin/users/list').params.get('q')).toBe('ali')
    await page.getByRole('button', {name: 'Clear search'}).click()
    await expect(page.getByText('bob@example.com')).toBeVisible()

    await page.getByRole('button', {name: 'Viewer, 1 user'}).click()
    await expect(page.getByText('user3@example.com')).toBeVisible()
    expect(last(calls, '/api/admin/users/list').params.get('role')).toBe('viewer')
    await expect(page.getByRole('button', {name: 'Viewer, 1 user'})).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', {name: 'Guest, 0 users'})).toBeDisabled()
    await page.getByRole('button', {name: 'All, 17 users'}).click()
    await expect(page.getByText('bob@example.com')).toBeVisible()

    await page.getByRole('navigation', {name: 'Users pagination'}).getByRole('button', {name: 'Next page'}).click()
    const unassigned = page.getByLabel('Change role for user17@example.com')
    await expect(unassigned).toHaveValue('')
    await expect(page.getByRole('cell', {name: 'Unassigned'})).toBeVisible()
    await page.getByRole('navigation', {name: 'Users pagination'}).getByRole('button', {name: 'Previous page'}).click()

    await page.getByLabel('Change role for bob@example.com').selectOption('viewer')
    const dialog = page.getByRole('dialog')
    await expect(dialog).toContainText('Change bob@example.com from Member to Viewer?')
    await dialog.getByRole('button', {name: 'Change role'}).click()
    await expect(page.getByText('Role updated')).toBeVisible()
    const put = calls.find(c => c.method === 'PUT')!
    expect(put.path).toBe('/api/admin/users/u2/role')
    expect(put.body).toEqual({role: 'viewer'})
  })

  test('pages the audit log by fifteen with an exact next-page signal and filters by action', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    const calls = await stubAdmin(page)
    await openAdmin(page)

    const first = calls.find(c => c.path === '/api/admin/audit')!
    expect(first.params.get('limit')).toBe('16')
    expect(first.params.get('offset')).toBe('0')

    const nav = page.getByRole('navigation', {name: 'Audit log pagination'})
    await expect(nav).toContainText('Showing 1–15')
    await nav.getByRole('button', {name: 'Next page'}).click()
    await expect(nav).toContainText('Showing 16–30')
    expect(last(calls, '/api/admin/audit').params.get('offset')).toBe('15')
    await nav.getByRole('button', {name: 'Next page'}).click()
    await expect(nav).toContainText('Showing 31–40')
    await expect(nav.getByRole('button', {name: 'Next page'})).toBeDisabled()

    await page.getByRole('combobox', {name: 'Filter by action'}).selectOption('auth.login_failure')
    await expect(page.getByRole('cell', {name: 'Failed sign-in'}).first()).toBeVisible()
    const filtered = last(calls, '/api/admin/audit')
    expect(filtered.params.get('action')).toBe('auth.login_failure')
    expect(filtered.params.get('offset')).toBe('0')
    await expect(nav).toContainText('Showing 1–8')

    await page.getByRole('button', {name: 'Show details'}).first().click()
    await expect(page.getByText('invalid_password')).toBeVisible()
  })

  test('jumps between sections from the nav and keeps the shell still', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 900})
    await stubAdmin(page)
    await openAdmin(page)

    const nav = page.getByRole('navigation', {name: 'Admin sections'})
    await nav.getByRole('button', {name: 'Audit log'}).click()
    await expect(nav.getByRole('button', {name: 'Audit log'})).toHaveAttribute('aria-current', 'location')
    const heading = page.getByRole('heading', {level: 2, name: 'Audit log'})
    await expect(heading).toBeFocused()
    await expect(heading).toBeInViewport()
    await shellStill(page)

    await nav.getByRole('button', {name: 'Users'}).click()
    await expect(page.getByRole('heading', {level: 2, name: 'Users'})).toBeInViewport()
    await page.getByRole('button', {name: 'View activity'}).first().click()
    await expect(page.getByRole('textbox', {name: 'Filter by user ID'})).toHaveValue('user-1')
    await expect(heading).toBeInViewport()
    await expect(page.getByRole('cell', {name: 'Failed sign-in'}).first()).toBeVisible()
    await shellStill(page)
  })

  test('lays out to the pane width without sideways overflow', async ({page}) => {
    await stubAdmin(page)
    await page.setViewportSize({width: WIDTHS[0], height: 900})
    await openAdmin(page)
    for (const width of WIDTHS) {
      await page.setViewportSize({width, height: 900})
      await expect(page.getByRole('heading', {level: 1, name: 'Admin Dashboard'})).toBeVisible()
      const m = await page.evaluate(() => {
        const root = document.querySelector<HTMLElement>('.admin-page')!
        const nav = document.querySelector<HTMLElement>('.admin-nav')!
        return {
          paneWidth: root.clientWidth,
          overflow: root.scrollWidth - root.clientWidth,
          navDirection: getComputedStyle(nav).flexDirection,
        }
      })
      expect(m.overflow, `sideways overflow at ${width}px`).toBe(0)
      expect(m.navDirection, `nav at ${width}px (pane ${m.paneWidth}px)`).toBe(m.paneWidth >= 880 ? 'column' : 'row')
    }
  })
})
