import {test, expect, type Page} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'
import {mockProviderCatalog} from '../src/testing/fixtures/providerCatalog'

// AI Accounts driven by GET /api/providers/catalog: one section per entry in
// the server's order; a known api_key provider gets the key form, a provider
// this bundle does not know gets the generic card, and a field type it does
// not know never becomes an input.

async function openAccounts(page: Page) {
  await page.locator('button[aria-label="Settings"]').first().click()
  const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
  await expect(dialog).toBeVisible({timeout: 5000})
  await dialog.evaluate(el => Promise.all(el.getAnimations().map(a => a.finished)))
  await dialog.getByRole('tab', {name: 'AI Accounts'}).click()
  await expect(dialog.getByRole('heading', {level: 2, name: 'AI Accounts'})).toBeVisible()
  return dialog
}

test.describe('AI Accounts from the provider catalogue', () => {
  test.beforeEach(async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
    await page.route('**/api/providers/catalog', route => route.fulfill({status: 200, json: mockProviderCatalog()}))
    // The real envelope: read as a bare boolean it was always truthy (C2).
    await page.route('**/api/keys/has', route => route.fulfill({status: 200, json: {has: false}}))
    await page.route('**/api/keys/save', route => route.fulfill({status: 200, json: {status: 'ok'}}))
    await page.route('**/api/providers/test', route => route.fulfill({status: 200, json: {ok: true, latencyMs: 7}}))
    await page.goto('/')
    await page.waitForLoadState('networkidle')
  })

  test('renders the sections in catalogue order and saves a key as {provider, key}', async ({page}) => {
    const dialog = await openAccounts(page)
    const headings = dialog.locator('#settings-panel').getByRole('heading', {level: 3})
    await expect(headings).toHaveText(['Claude', 'Mistral', 'Quantum AI'])
    await expect(dialog.locator('#provider-claude')).toBeVisible()

    const claude = dialog.locator('section[data-provider="claude"]')
    const save = claude.getByRole('button', {name: 'Save'})
    await expect(save).toBeDisabled()
    await claude.getByLabel('API key', {exact: true}).fill('sk-test')
    const saveRequest = page.waitForRequest(r => r.url().endsWith('/api/keys/save'))
    await save.click()
    expect((await saveRequest).postDataJSON()).toEqual({provider: 'claude', key: 'sk-test'})
    await expect(claude.getByText(/API key configured/)).toBeVisible()
    await expect(claude.getByRole('button', {name: 'Remove'})).toBeVisible()
  })

  test('a provider the bundle does not know is a generic card; an unknown field type is never an input', async ({
    page,
  }) => {
    const dialog = await openAccounts(page)
    const mistral = dialog.locator('section[data-provider="mistral"]')
    await expect(mistral.getByText('Configured', {exact: true})).toBeVisible()
    await expect(mistral.getByRole('link', {name: 'Set-up guide'})).toHaveAttribute(
      'href',
      'https://docs.example.com/mistral',
    )
    await expect(mistral.locator('input')).toHaveCount(0)
    await mistral.getByRole('button', {name: 'Test'}).click()
    await expect(mistral.getByText('Valid · 7 ms')).toBeVisible()

    const quantum = dialog.locator('section[data-provider="quantum"]')
    await expect(quantum.getByText("This version of Baki can't show the set-up form for this provider.")).toBeVisible()
    await expect(quantum.getByText('This provider is set up by a newer version of Baki.')).toBeVisible()
    await expect(quantum.locator('input, textarea, select')).toHaveCount(0)
    await expect(quantum.getByRole('button', {name: 'Test'})).toHaveCount(0)
  })
})
