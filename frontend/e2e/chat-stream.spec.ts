import {test, expect, Page, Route} from '@playwright/test'
import {
  mockFlowDocument,
  mockAnalysisReport,
  mockDashboardHome,
  setupAuthenticatedPage,
  SAMPLE_FLOW_CONTENT,
} from './helpers'
import {chatEventGolden} from '../src/testing/chatEventGoldens'

const STREAM_ID = 'e2e-fixed-stream-id'

function sseBody(events: Array<{name: string; data: unknown}>) {
  return events.map(e => `event: ${e.name}\ndata: ${JSON.stringify(e.data)}\n\n`).join('') + '\n'
}

type SseEvent = {name: string; data: unknown}

interface ChatBootOptions {
  sse?: SseEvent[]
  resume?: unknown
}

/** A backend event golden (internal/service/testdata/chat_events, R-testing-3) on the e2e stream. */
function golden(name: string): SseEvent {
  return {name: 'chat:event', data: chatEventGolden(name, STREAM_ID)}
}

const DEFAULT_SSE: SseEvent[] = [golden('tool'), golden('chunk'), golden('done')]

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
            supportsTools: true,
          },
        ],
        defaultModel: 'claude-haiku-4-5',
      },
    ],
    '/api/chat/get': [],
    '/api/chat/suggested-prompts': ['Summarize this flow'],
    '/api/flow/source-files': [],
    '/api/flow/upload': mockFlowDocument(),
    '/api/chat/stream': STREAM_ID,
    '/api/chat/save': {status: 'ok'},
    '/api/chat/cancel': {status: 'ok'},
    '/api/chat/resume': options.resume ?? {...chatEventGolden('resume'), events: []},
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

test.describe('Fix card', () => {
  test('approving a batch with one fix deselected posts its index', async ({page}) => {
    const proposal = golden('fix_proposal_batch')
    const request = (proposal.data as {data: Record<string, unknown>}).data
    await stubChatBoot(page, {
      sse: [golden('chunk'), proposal],
      resume: {
        text: 'Fixed it.',
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
    await input.fill('Fix the HTTP calls')
    await input.press('Enter')

    const card = page.getByRole('alertdialog', {name: 'Proposed 2 fixes: approval needed'})
    await expect(card).toBeVisible({timeout: 10_000})
    await card.getByRole('checkbox', {name: 'Include fix for Invoke Url'}).nth(1).uncheck()
    const decision = page.waitForRequest(req => req.url().endsWith('/api/chat/fix-decision') && req.method() === 'POST')
    await card.getByRole('button', {name: 'Approve & apply 1 fix'}).click()
    const body = (await decision).postDataJSON() as Record<string, unknown>
    expect(body).toMatchObject({
      streamId: STREAM_ID,
      proposalId: 'proposal-1',
      approved: true,
      excludedItemIndices: [1],
    })
  })
})

// R-frontend-6: a new chat runs with tools on; the header says what the request sends.
test.describe('Tools default', () => {
  test('a new chat sends useTools: true, the header says Tools on, and switching off shows the composer hint', async ({
    page,
  }) => {
    await stubChatBoot(page)
    await openChatWithFlow(page)

    const chat = page.locator('.chat-root')
    const input = chat.locator('textarea[placeholder*="Ask anything"]')
    await input.waitFor({timeout: 10_000})
    await expect(chat.getByText(/Tools are off/)).toBeHidden()

    await chat.getByRole('button', {name: 'More actions'}).click()
    await expect(page.getByRole('menuitem', {name: /^Tools on/})).toBeVisible()
    await page.keyboard.press('Escape')

    const stream = page.waitForRequest(req => req.url().endsWith('/api/chat/stream') && req.method() === 'POST')
    await input.fill('Which blocks call the API?')
    await input.press('Enter')
    expect(((await stream).postDataJSON() as Record<string, unknown>).useTools).toBe(true)

    await chat.getByRole('button', {name: 'More actions'}).click()
    await page.getByRole('menuitem', {name: /^Tools on/}).click()
    await expect(chat.getByText(/Tools are off/)).toBeVisible()
    await chat.getByRole('button', {name: 'More actions'}).click()
    await expect(page.getByRole('menuitem', {name: /^Tools off/})).toBeVisible()
  })
})
