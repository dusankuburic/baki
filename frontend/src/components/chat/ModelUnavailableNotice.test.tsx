import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from '@/i18n'
import ModelUnavailableNotice, {modelNoticeSentences, type ModelNoticeView} from './ModelUnavailableNotice'

const DOCS = {provider: 'azure-foundry', model: 'm/docs000000', label: 'Docs writer'}
const GPT = {provider: 'azure-foundry', model: 'm/gpt4100000', label: 'GPT-4.1'}
const providerName = (id: string) => ({'azure-foundry': 'Azure AI Foundry', openai: 'OpenAI'})[id] ?? id

function renderNotice(notice: ModelNoticeView) {
  const handlers = {onChooseModel: vi.fn(), onDismiss: vi.fn(), onManage: vi.fn()}
  const view = render(<ModelUnavailableNotice notice={notice} providerName={providerName} {...handlers} />)
  return {...view, ...handlers}
}

describe('ModelUnavailableNotice', () => {
  it('a removed model: names the stand-in; Choose model opens the picker, Dismiss keeps the stand-in', async () => {
    const user = userEvent.setup()
    const {onChooseModel, onDismiss} = renderNotice({kind: 'removed', wanted: DOCS, fallback: GPT})
    expect(screen.getByText('“Docs writer” is no longer available. This chat now uses “GPT-4.1”.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', {name: 'Choose model'}))
    expect(onChooseModel).toHaveBeenCalledTimes(1)
    await user.click(screen.getByRole('button', {name: 'Dismiss'}))
    expect(onDismiss).toHaveBeenCalledTimes(1)
  })

  it('a removed model with nothing to stand in: no Dismiss (there is nothing to keep)', () => {
    renderNotice({kind: 'removed', wanted: DOCS, fallback: null})
    expect(
      screen.getByText('“Docs writer” is no longer available, and no other model can be used here.'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Dismiss'})).toBeNull()
    expect(screen.getByRole('button', {name: 'Choose model'})).toBeInTheDocument()
  })

  it('a temporarily unavailable model: says why and what stands in, and is never dismissed (the thread keeps it)', () => {
    renderNotice({kind: 'unavailable', wanted: DOCS, fallback: GPT, cause: 'needs_credentials'})
    expect(
      screen.getByText("“Docs writer” is unavailable until its resource's credentials are entered again."),
    ).toBeInTheDocument()
    expect(screen.getByText('This chat uses “GPT-4.1” until then.')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Choose model'})).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Dismiss'})).toBeNull()
  })

  it('names each cause', () => {
    const t = i18next.getFixedT('en', 'chat')
    const sentence = (notice: ModelNoticeView) => modelNoticeSentences(t, notice, providerName)[0]
    const wanted = DOCS
    expect(sentence({kind: 'unavailable', wanted, fallback: null, cause: 'unreadable'})).toContain(
      'credentials are entered again',
    )
    expect(sentence({kind: 'unavailable', wanted, fallback: null, cause: 'invalid_config'})).toBe(
      "“Docs writer” is unavailable until its resource's settings are fixed.",
    )
    expect(sentence({kind: 'unavailable', wanted, fallback: null, cause: 'unsupported'})).toContain(
      'agents need Microsoft Entra ID and a project endpoint',
    )
    expect(
      sentence({
        kind: 'unavailable',
        wanted: {provider: 'openai', model: 'gpt-5', label: 'GPT-5'},
        fallback: null,
        cause: 'provider',
      }),
    ).toBe("“GPT-5” is unavailable here: OpenAI isn't set up for this flow.")
    expect(sentence({kind: 'refused', wanted, fallback: null})).toBe(
      "“Docs writer” isn't available any more. Choose another model to send your message.",
    )
  })

  it('a saved default that names nothing: says what new chats use, offers Manage models… and Dismiss', async () => {
    const user = userEvent.setup()
    const {onManage, onDismiss} = renderNotice({kind: 'legacyDefault', saved: 'gpt-4o-old', fallback: 'GPT-4.1'})
    expect(
      screen.getByText(
        'Your saved default model “gpt-4o-old” is no longer in your model list, so new Azure AI Foundry chats start on “GPT-4.1”.',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Choose model'})).toBeNull()
    await user.click(screen.getByRole('button', {name: 'Manage models…'}))
    await user.click(screen.getByRole('button', {name: 'Dismiss'}))
    expect(onManage).toHaveBeenCalledTimes(1)
    expect(onDismiss).toHaveBeenCalledTimes(1)
  })

  // The chat panel's always-mounted status region announces it; the row
  // itself would re-announce on every re-render if it were live.
  it('is not a live region', () => {
    const {container} = renderNotice({kind: 'removed', wanted: DOCS, fallback: GPT})
    expect(container.querySelector('[role="status"], [role="alert"], [aria-live]')).toBeNull()
  })
})
