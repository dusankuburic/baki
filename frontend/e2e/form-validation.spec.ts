import {test, expect, Page, Route} from '@playwright/test'
import {setupAuthenticatedPage, mockDashboardHome} from './helpers'

async function stubLoginGate(page: Page) {
  await page.route('**/api/local-config', (route: Route) =>
    route.fulfill({status: 200, json: {mode: 'cloud'}}),
  )
}

test.describe('Login field validation', () => {
  test('a malformed email is flagged inline after blur, without blocking login tolerance', async ({page}) => {
    await stubLoginGate(page)
    const logins: unknown[] = []
    await page.route('**/api/auth/login', route => {
      logins.push(route.request().url())
      return route.fulfill({status: 401, json: {error: 'invalid credentials'}})
    })

    await page.goto('/')
    const email = page.getByLabel('Email', {exact: true})
    await expect(email).toBeVisible({timeout: 10_000})

    await email.fill('not-an-email')
    await expect(page.getByText('Enter a valid email address')).toBeHidden()
    await email.blur()
    await expect(page.getByText('Enter a valid email address')).toBeVisible()
    await expect(email).toHaveAttribute('aria-invalid', 'true')

    await email.fill('someone@example.com')
    await email.blur()
    await expect(page.getByText('Enter a valid email address')).toBeHidden()
    expect(logins).toHaveLength(0)
  })
})

test.describe('Register field validation', () => {
  test('the password policy runs live, mismatches show inline, and an invalid form sends nothing', async ({page}) => {
    await stubLoginGate(page)
    const registrations: unknown[] = []
    await page.route('**/api/auth/register', route => {
      registrations.push(route.request().postDataJSON())
      return route.fulfill({
        status: 200,
        json: {
          user: {id: 'u1', email: 'e2e@example.com', role: 'member', displayName: 'E2E'},
          verificationRequired: true,
        },
      })
    })

    await page.goto('/')
    const email = page.getByLabel('Email', {exact: true})
    await expect(email).toBeVisible({timeout: 10_000})
    await page.getByRole('button', {name: 'Sign up', exact: true}).click()

    const password = page.getByLabel('Password', {exact: true})
    const confirm = page.getByLabel('Confirm Password')
    const submit = page.getByRole('button', {name: 'Sign up', exact: true})

    await email.fill('e2e@example.com')
    await password.fill('short')
    const rules = page.getByRole('list', {name: 'Password requirements'})
    await expect(rules).toBeVisible()
    await expect(rules.getByText('At least 12 characters')).toBeVisible()
    expect(await rules.locator('li[data-met="true"]').count()).toBe(0)
    await expect(submit).toBeDisabled()

    await password.fill('Correct-horse-1')
    await expect(rules.locator('li[data-met="true"]')).toHaveCount(2)
    await confirm.fill('Different-horse-1')
    await confirm.blur()
    await expect(page.getByText('Passwords do not match')).toBeVisible()
    await expect(confirm).toHaveAttribute('aria-invalid', 'true')

    await confirm.fill('Correct-horse-1')
    await confirm.blur()
    await expect(page.getByText('Passwords do not match')).toBeHidden()
    await email.fill('not-an-email')
    await expect(submit).toBeDisabled()
    expect(registrations).toHaveLength(0)

    await email.fill('e2e@example.com')
    await expect(submit).toBeEnabled()
    await submit.click()
    await expect(page.getByText('Check your inbox')).toBeVisible({timeout: 10_000})
    expect(registrations).toHaveLength(1)
    expect(registrations[0]).toMatchObject({email: 'e2e@example.com', password: 'Correct-horse-1'})
  })
})

test.describe('Settings field validation', () => {
  test('the token expiry error is inline and aria-wired, not a toast', async ({page}) => {
    await setupAuthenticatedPage(page)
    await page.route('**/api/dashboard/home', route => route.fulfill({status: 200, json: mockDashboardHome()}))
    await page.route('**/api/auth/tokens', route => route.fulfill({status: 200, json: []}))
    await page.goto('/')
    await page.waitForLoadState('networkidle')

    await page.locator('button[aria-label="Settings"]').first().click()
    const dialog = page.locator('[role="dialog"][aria-label="Settings"]')
    await expect(dialog).toBeVisible({timeout: 5000})
    await dialog.getByRole('tab', {name: 'API Tokens'}).click()

    const expiry = dialog.getByLabel('Token expiry in days (0 for never)')
    await expect(expiry).toBeVisible()

    await expiry.fill('30.5')
    await expiry.blur()
    await expect(
      dialog.getByText('Expiry must be a whole number of days, at most 365 — or empty for no expiry.'),
    ).toBeVisible()
    await expect(expiry).toHaveAttribute('aria-invalid', 'true')

    await expiry.fill('400')
    await expect(
      dialog.getByText('Expiry must be a whole number of days, at most 365 — or empty for no expiry.'),
    ).toBeVisible()
    await expiry.fill('30')
    await expect(
      dialog.getByText('Expiry must be a whole number of days, at most 365 — or empty for no expiry.'),
    ).toBeHidden()
  })
})
