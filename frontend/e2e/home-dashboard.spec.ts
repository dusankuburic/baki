import {test, expect, Page, Route} from '@playwright/test'
import {setupAuthenticatedPage} from './helpers'
import {chartLayoutMetrics, WIDTHS} from './chartChecks'
import {mockDashboardAnalytics} from '../src/testing/fixtures/dashboardAnalytics'

function day(offset: number): string {
  const d = new Date()
  d.setUTCHours(0, 0, 0, 0)
  d.setDate(d.getDate() + offset)
  return d.toISOString().slice(0, 10)
}

const RULES = [
  'missing-error-handling',
  'hardcoded-credential',
  'missing-timeout',
  'unused-variable',
  'deprecated-action',
  'empty-catch-block',
  'hardcoded-url',
  'no-retry-policy',
  'uninitialized-variable',
  'long-subflow',
]

function homePayload() {
  return {
    greeting: {userDisplayName: 'Tester', activeOrgName: ''},
    overview: {avgHealthScore: 72, healthAvailable: true, totalFlows: 4, totalSubflows: 19},
    tokenUsage: Array.from({length: 14}, (_, i) => ({
      date: day(i - 13),
      tokensIn: i % 2 ? 0 : 1200 + i * 40,
      tokensOut: i % 2 ? 0 : 400 + i * 10,
    })),
    recentFlows: [
      {id: 'flow-1', name: 'Invoice sync', healthScore: 42, updatedAt: new Date(Date.now() - 3600e3).toISOString()},
      {
        id: 'flow-2',
        name: 'Customer onboarding',
        healthScore: 61,
        updatedAt: new Date(Date.now() - 7200e3).toISOString(),
      },
      {
        id: 'flow-3',
        name: 'Nightly export',
        healthScore: null,
        updatedAt: new Date(Date.now() - 86400e3).toISOString(),
      },
    ],
    findings: {
      available: true,
      bySeverity: {error: 6, warning: 14, info: 10},
      byCategory: [
        {category: 'Reliability', count: 18},
        {category: 'Security', count: 8},
        {category: 'Performance', count: 4},
      ],
    },
    isCloud: true,
    healthTrend: Array.from({length: 30}, (_, i) => ({
      date: day(i - 29),
      avgHealth: i < 3 ? 0 : 60 + Math.round(Math.sin(i / 4) * 12),
      flowCount: i < 3 ? 0 : 4,
    })),
    ruleFrequency: RULES.map((rule, i) => ({
      rule,
      count: 20 - i * 2,
      topSeverity: i % 3 === 0 ? 'error' : i % 3 === 1 ? 'warning' : 'info',
    })),
    activity: [
      {action: 'flow.analyze', flowName: 'Invoice sync', createdAt: new Date(Date.now() - 3600e3).toISOString()},
      {action: 'auth.login', createdAt: new Date(Date.now() - 7200e3).toISOString()},
      {action: 'settings.update', createdAt: new Date(Date.now() - 86400e3).toISOString()},
    ],
    complexity: [],
    security: {available: true, failedLogins24h: 1, credentialFindings: 8, lockedAccounts: 0, admin: true},
    severityTrend: Array.from({length: 30}, (_, i) => ({
      date: day(i - 29),
      errors: 6,
      warnings: 20 - Math.round(i / 5),
      info: 10,
    })),
    confidenceDist: {high: 18, medium: 9, low: 3},
    healthBuckets: [
      {label: '0-20', lo: 0, hi: 20, count: 0},
      {label: '20-40', lo: 20, hi: 40, count: 0},
      {label: '40-60', lo: 40, hi: 60, count: 1},
      {label: '60-80', lo: 60, hi: 80, count: 1},
      {label: '80-100', lo: 80, hi: 100, count: 2},
    ],
    fixability: {available: 12, total: 30, autoFixableRules: 11, totalRules: 44},
    workflow: {
      available: true,
      funnel: {open: 11, acknowledged: 4, in_progress: 3, resolved: 7, suppressed: 2},
      mttrHours: 16.5,
      resolvedCount: 7,
      staleCount: 3,
    },
  }
}

async function stubDashboards(page: Page) {
  await setupAuthenticatedPage(page)
  await page.route('**/api/dashboard/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/dashboard/home') {
      await route.fulfill({status: 200, json: homePayload()})
      return
    }
    if (url.pathname === '/api/dashboard/analytics') {
      await route.fulfill({status: 200, json: mockDashboardAnalytics()})
      return
    }
    await route.fallback()
  })
}

async function openHome(page: Page) {
  await page.goto('/')
  await page.getByRole('button', {name: 'Home Dashboard'}).first().click()
  await expect(page.getByRole('heading', {name: /Tester/})).toBeVisible({timeout: 10_000})
  await expect(page.locator('svg.recharts-surface').first()).toBeVisible()
}

function card(page: Page, title: string) {
  return page.locator('.chart-card', {has: page.getByRole('heading', {name: title, exact: true})})
}

test.describe('Home dashboard', () => {
  test('shows the overview with tiles, cards and their table twins', async ({page}) => {
    await page.setViewportSize({width: 1600, height: 1000})
    await stubDashboards(page)
    await openHome(page)

    await expect(page.getByText('Fair · of 100')).toBeVisible()
    await expect(page.getByText('72', {exact: true}).first()).toBeVisible()
    await expect(page.getByText("Couldn't load your dashboard")).toHaveCount(0)
    await expect(page.getByText('Something went wrong')).toHaveCount(0)

    for (const title of [
      'Health trend',
      'Findings by category',
      'Top rules',
      'Triage',
      'Recent flows',
      'Recent activity',
      'AI usage',
      'Security posture',
    ]) {
      await expect(card(page, title)).toBeVisible()
    }

    const rules = card(page, 'Top rules')
    await expect(rules.locator('.recharts-yAxis .recharts-cartesian-axis-tick')).toHaveCount(10)
    await rules.getByRole('radio', {name: 'Table'}).click()
    await expect(rules.getByRole('row')).toHaveCount(11)
    await expect(rules.getByText('Error', {exact: true}).first()).toBeVisible()
    await rules.getByRole('radio', {name: 'Chart'}).click()

    const activity = card(page, 'Recent activity')
    await expect(activity.getByText('Analyzed flow')).toBeVisible()
    await expect(activity.getByText('Updated settings')).toBeVisible()
    await expect(activity.getByText(/setting-row|resource/)).toHaveCount(0)

    await expect(card(page, 'Security posture').getByText('Locked accounts')).toBeVisible()
  })

  test('"Details" opens the Analytics page at the matching section', async ({page}) => {
    await page.setViewportSize({width: 1400, height: 900})
    await stubDashboards(page)
    await openHome(page)

    await page.getByRole('button', {name: 'Open Health trend in analytics'}).click()
    await expect(page.getByRole('heading', {name: 'Analytics', level: 1})).toBeVisible()
    await expect(page.getByRole('heading', {name: 'Trends'})).toBeInViewport()
    const doc = await page.evaluate(() => ({
      top: document.documentElement.scrollTop,
      height: document.documentElement.scrollHeight,
      viewport: window.innerHeight,
    }))
    expect(doc.top).toBe(0)
    expect(doc.height).toBe(doc.viewport)
    await expect(page.locator('.main-toolbar').getByRole('radio', {name: 'Analytics'})).toBeChecked()
  })

  test('keeps every card and axis tick inside the pane at any width', async ({page}) => {
    await stubDashboards(page)
    for (const width of WIDTHS) {
      await page.setViewportSize({width, height: 900})
      if (width === WIDTHS[0]) await openHome(page)
      await expect
        .poll(() => chartLayoutMetrics(page, '.home-dashboard'), {message: `${width}px`, timeout: 5_000})
        .toMatchObject({pageOverflow: 0, spilling: 0, clipped: 0})
      const m = await chartLayoutMetrics(page, '.home-dashboard')
      expect(m.cards, `${width}px: cards`).toBeGreaterThan(8)
      expect(m.ticks, `${width}px: ticks`).toBeGreaterThan(10)
    }
  })
})
