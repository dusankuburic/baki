import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import type {DeploymentAISettings} from '@/api/admin'

const getDeploymentAISettings = vi.fn()
const updateDeploymentAISettings = vi.fn()
const getProviderCatalog = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    getDeploymentAISettings: (...a: unknown[]) => getDeploymentAISettings(...a),
    updateDeploymentAISettings: (...a: unknown[]) => updateDeploymentAISettings(...a),
  },
}))
vi.mock('@/api', () => ({
  providersApi: {getProviderCatalog: (...a: unknown[]) => getProviderCatalog(...a)},
}))

import {AILimitsCard} from './AILimitsCard'

const ENFORCED: DeploymentAISettings = {dailyBudget: 0, perUserDailyBudget: 0, embeddingProvider: 'openai', embeddingModel: ''}

function renderCard() {
  return render(
    <ToastProvider>
      <AILimitsCard />
    </ToastProvider>,
  )
}

// R-config-1: the deployment-wide budgets and embedding choice had no writer in cloud; the admin page now shows
// the enforced values and writes them through PUT /api/admin/settings/ai.
describe('AILimitsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    // A fresh object per read, as a fetch answers.
    getDeploymentAISettings.mockImplementation(() => Promise.resolve({...ENFORCED}))
    updateDeploymentAISettings.mockImplementation((body: DeploymentAISettings) => Promise.resolve(body))
    getProviderCatalog.mockResolvedValue([
      {id: 'openai', name: 'OpenAI', caps: ['chat', 'embeddings']},
      {id: 'claude', name: 'Claude', caps: ['chat']},
    ])
  })

  it('shows the enforced values and saves an edited budget', async () => {
    renderCard()
    const budget = (await screen.findByLabelText('Daily budget (USD)')) as HTMLInputElement
    expect(budget.value).toBe('0')

    fireEvent.change(budget, {target: {value: '200'}})
    fireEvent.blur(budget)
    fireEvent.click(screen.getByRole('button', {name: 'Save limits'}))

    await waitFor(() => expect(updateDeploymentAISettings).toHaveBeenCalledWith({...ENFORCED, dailyBudget: 200}))
    expect(await screen.findByText('AI limits saved')).toBeInTheDocument()
  })

  // The budget fields commit on blur, and a click on a disabled Save does not blur the field in every browser:
  // Save stays enabled while a budget is being typed, so the first click after typing commits and saves.
  it('keeps Save enabled while a budget is still being typed, and saves nothing when clean', async () => {
    renderCard()
    const budget = await screen.findByLabelText('Daily budget (USD)')
    const save = screen.getByRole('button', {name: 'Save limits'})

    fireEvent.click(save)
    expect(updateDeploymentAISettings).not.toHaveBeenCalled()

    fireEvent.change(budget, {target: {value: '200'}})
    expect(save).toBeEnabled()
  })

  // The answer to the PUT is what the server now enforces: a refetch could land on a replica whose settings cache
  // has not yet seen the write and would reset the form to the old values.
  it('takes the enforced values from the save response, not a refetch', async () => {
    renderCard()
    const budget = (await screen.findByLabelText('Daily budget (USD)')) as HTMLInputElement
    fireEvent.change(budget, {target: {value: '200'}})
    fireEvent.blur(budget)
    fireEvent.click(screen.getByRole('button', {name: 'Save limits'}))

    expect(await screen.findByText('AI limits saved')).toBeInTheDocument()
    expect(budget.value).toBe('200')
    expect(getDeploymentAISettings).toHaveBeenCalledTimes(1)
  })

  // A provider whose model ids are deployment aliases (Azure AI Foundry) embeds only on the operator's
  // PAD_EMBEDDING_PROVIDER pin; the server refuses it here, so the card does not offer it.
  it('offers only providers that can embed without the operator pin', async () => {
    getProviderCatalog.mockResolvedValue([
      {id: 'openai', name: 'OpenAI', caps: ['chat', 'embeddings']},
      {id: 'claude', name: 'Claude', caps: ['chat']},
      {id: 'azure-foundry', name: 'Azure AI Foundry', caps: ['chat', 'embeddings', 'model_aliases']},
    ])
    renderCard()
    const select = (await screen.findByLabelText('Embedding provider')) as HTMLSelectElement
    await waitFor(() => expect([...select.options].map(o => o.value)).toEqual(['', 'openai']))
  })

  it('says when the provider list could not be loaded and retries it', async () => {
    getProviderCatalog.mockRejectedValueOnce(new Error('network'))
    renderCard()
    const select = (await screen.findByLabelText('Embedding provider')) as HTMLSelectElement
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load the provider list")
    expect(select).toBeDisabled()
    expect([...select.options].map(o => o.value)).toEqual(['', 'openai'])

    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    await waitFor(() => expect(select).toBeEnabled())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(getProviderCatalog).toHaveBeenCalledTimes(2)
  })

  it('keeps a failed save on the card', async () => {
    updateDeploymentAISettings.mockRejectedValueOnce(new Error('ai.dailyBudget must be a non-negative amount'))
    renderCard()
    const budget = await screen.findByLabelText('Per-user daily budget (USD)')
    fireEvent.change(budget, {target: {value: '5'}})
    fireEvent.blur(budget)
    fireEvent.click(screen.getByRole('button', {name: 'Save limits'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('non-negative')
  })
})
