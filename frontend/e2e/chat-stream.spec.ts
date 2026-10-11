import {test, expect, Page, Route} from '@playwright/test'
import {
  mockFlowDocument,
  mockAnalysisReport,
  mockDashboardHome,
  setupAuthenticatedPage,
  SAMPLE_FLOW_CONTENT,
} from './helpers'

const STREAM_ID = 'e2e-fixed-stream-id'

function sseBody(events: Array<{name: string; data: unknown}>) {
  return events.map(e => `event: ${e.name}\ndata: ${JSON.stringify(e.data)}\n\n`).join('') + '\n'
}

type SseEvent = {name: string; data: unknown}

interface ChatBootOptions {
  sse?: SseEvent[]
  resume?: unknown
}

const DEFAULT_SSE: SseEvent[] = [
  {name: 'chat:event', data: {streamId: STREAM_ID, type: 'tool', data: {label: 'analyzing'}}},
  {name: 'chat:event', data: {streamId: STREAM_ID, type: 'chunk', data: {content: 'This flow has '}}},
  {name: 'chat:event', data: {streamId: STREAM_ID, type: 'chunk', data: {content: '1 error to fix.'}}},
  {name: 'chat:event', data: {streamId: STREAM_ID, type: 'done', data: {tokensIn: 42, tokensOut: 7}}},
]

async function stubChatBoot(page: Page, options: ChatBootOptions = {}) {
  await setupAuthenticatedPage(page)
  await page.addInitScript(() => {
    Object.defineProperty(crypto, 'randomUUID', {value: () => 'e2e-fixed-stream-id'})
  })

  const bootStubs: Record<string, unknown> = {
    '/api/local-config': {mode: 'cloud'},
    '/api/system/info': {
      version: 'e2e',
      platform: 'linux',
      arch: 'x64',
      buildDate: '',
      gitCommit: '',
      capabilities: {sessionAnalytics: false},
    },
    '/api/system/settings': {
      version: 1,
      general: {firstRunCompleted: true},
      appearance: {},
      layout: {},
      ai: {},
      parser: {},
      analysis: {},
    },
    '/api/flow/recent': [],
    '/api/orgs/': [],
    '/api/analysis/dashboard': mockDashboardHome(),
    '/api/flow/load-path': mockFlowDocument(),
    '/api/analysis/analyze': mockAnalysisReport(),
    '/api/auth/refresh': {accessToken: 'tok', expiresAt: '2099-01-01T00:00:00Z'},
    '/api/auth/me': {id: 'user-1', email: 'e2e@example.com', role: 'member', displayName: 'E2E'},
    '/api/providers/list': [
      {
        id: 'claude',
        name: 'Claude',
        configured: true,
        authType: 'api_key',
        contextLimit: 200000,
        models: [
          {
            id: 'claude-haiku-4-5',
            displayName: 'Claude Haiku 4.5',
            contextLimit: 200000,
            inputCostPerM: 1,
            outputCostPerM: 5,
            supportsTools: true,
          },
        ],
        defaultModel: 'claude-haiku-4-5',
      },
    ],
    '/api/chat/get': [],
    '/api/chat/suggested-prompts': ['Summarize this flow'],
    '/api/flow/source-files': [],
    '/api/chat/begin': {status: 'ok'},
    '/api/flow/upload': mockFlowDocument(),
    '/api/chat/stream': STREAM_ID,
    '/api/chat/save': {status: 'ok'},
    '/api/chat/cancel': {status: 'ok'},
    '/api/chat/resume': options.resume ?? {
      text: 'This flow has 1 error to fix.',
      done: true,
      error: '',
      tokensIn: 42,
      tokensOut: 7,
    },
    '/api/chat/fix-decision': {status: 'ok'},
    '/api/analysis/triage/list': [],
    '/api/analysis/baseline/get': null,
  }

  await page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/events') {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sseBody(options.sse ?? DEFAULT_SSE),
      })
      return
    }

    for (const [path, body] of Object.entries(bootStubs)) {
      if (url.pathname === path) {
        await route.fulfill({status: 200, json: body})
        return
      }
    }
    await route.fallback()
  })
}

async function openChatWithFlow(page: Page) {
  await page.goto('/')
  const openBtn = page.locator('button:has-text("Open file")').first()
  const [fileChooser] = await Promise.all([page.waitForEvent('filechooser'), openBtn.click()])
  await fileChooser.setFiles({
    name: 'Main.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from(SAMPLE_FLOW_CONTENT),
  })
  await expect(page.locator('text=Customer Data Sync').first()).toBeVisible({timeout: 10_000})
  const aiTab = page.getByRole('tab', {name: 'AI', exact: true})
  await aiTab.first().waitFor({timeout: 15_000})
  await aiTab.first().click()
}

test.describe('Chat streaming', () => {
  test('streams an assistant response chunk by chunk', async ({page}) => {
    await stubChatBoot(page)
    await openChatWithFlow(page)

    const input = page.locator('textarea[placeholder*="Ask anything"]')
    await input.waitFor({timeout: 10_000})
    await input.fill('Summarize the findings')
    await input.press('Enter')

    await expect(page.getByText('Summarize the findings').first()).toBeVisible({timeout: 10_000})
    await expect(page.getByRole('tab', {name: /Generating/})).toBeVisible({timeout: 10_000})

    const log = page.getByRole('log')
    await expect.poll(() => log.evaluate(el => el.scrollWidth - el.clientWidth)).toBe(0)
    const edges = await page.evaluate(() => {
      const bubble = document.querySelector('.chat-msg-item > *')!.getBoundingClientRect()
      const card = document.querySelector('[data-chat-composer]')!.closest('.rounded-xl')!.getBoundingClientRect()
      return [Math.round(bubble.left - card.left), Math.round(card.right - bubble.right)]
    })
    expect(edges).toEqual([0, 0])

    await page.getByRole('button', {name: 'Focus mode'}).click()
    const focus = page.getByRole('dialog', {name: 'AI Chat'})
    await expect(focus).toBeVisible()
    await page.keyboard.press('Escape')
    if (await focus.isVisible()) {
      await expect(page.getByRole('tab', {name: /Generating/})).toBeHidden({timeout: 10_000})
      await page.keyboard.press('Escape')
    }
    await expect(focus).toBeHidden()
  })
})

test.describe('Approval card', () => {
  test('asks for a decision on a request of an unknown preview kind', async ({page}) => {
    const request = {
      proposalId: 'approval-1',
      tool: 'rename_variable',
      previewKind: 'something-new',
      requiredScope: 'write',
      items: [{id: 'v1', label: 'Variable %Customer%', summary: 'Rename to %CustomerRecord%'}],
    }
    await stubChatBoot(page, {
      sse: [
        {name: 'chat:event', data: {streamId: STREAM_ID, type: 'chunk', data: {content: 'Renaming… '}}},
        {name: 'chat:event', data: {streamId: STREAM_ID, type: 'fix_proposal', data: request}},
      ],
      resume: {
        text: 'Renaming… ',
        done: false,
        error: '',
        tokensIn: 0,
        tokensOut: 0,
        events: [{type: 'fix_proposal', data: request}],
      },
    })
    await openChatWithFlow(page)

    const input = page.locator('textarea[placeholder*="Ask anything"]')
    await input.waitFor({timeout: 10_000})
    await input.fill('Rename the customer variable')
    await input.press('Enter')

    const card = page.getByRole('alertdialog', {name: 'rename_variable: approval needed'})
    await expect(card).toBeVisible({timeout: 10_000})
    await expect(card.getByText('Variable %Customer%')).toBeVisible()
    await expect(card.getByText('Rename to %CustomerRecord%')).toBeVisible()
    await expect(card.getByText(/requires write access/i)).toBeVisible()
    await expect(card.getByRole('button', {name: 'Approve'})).toBeVisible()

    const decision = page.waitForRequest(req => req.url().endsWith('/api/chat/fix-decision') && req.method() === 'POST')
    await card.getByRole('button', {name: 'Dismiss'}).click()
    const body = (await decision).postDataJSON() as Record<string, unknown>
    expect(body).toMatchObject({streamId: STREAM_ID, proposalId: 'approval-1', approved: false})
    await expect(card.getByText('Dismissed')).toBeVisible()
    await expect(card.getByRole('button', {name: 'Approve'})).toBeHidden()
  })
})
