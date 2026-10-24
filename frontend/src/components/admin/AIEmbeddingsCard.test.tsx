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

import {AIEmbeddingsCard} from './AIEmbeddingsCard'

const STORED: DeploymentAISettings = {embeddingProvider: 'openai', embeddingModel: ''}

function renderCard() {
  return render(
    <ToastProvider>
      <AIEmbeddingsCard />
    </ToastProvider>,
  )
}

// R-config-1: the deployment-wide embedding choice had no writer in cloud; the admin page shows the stored values
// and writes them through PUT /api/admin/settings/ai.
describe('AIEmbeddingsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    // A fresh object per read, as a fetch answers.
    getDeploymentAISettings.mockImplementation(() => Promise.resolve({...STORED}))
    updateDeploymentAISettings.mockImplementation((body: DeploymentAISettings) => Promise.resolve(body))
    getProviderCatalog.mockResolvedValue([
      {id: 'openai', name: 'OpenAI', caps: ['chat', 'embeddings']},
      {id: 'claude', name: 'Claude', caps: ['chat']},
    ])
  })

  it('shows the stored values and saves an edited model', async () => {
    renderCard()
    const model = (await screen.findByLabelText('Embedding model')) as HTMLInputElement
    expect(model.value).toBe('')

    fireEvent.change(model, {target: {value: 'text-embedding-3-large'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save embeddings'}))

    await waitFor(() =>
      expect(updateDeploymentAISettings).toHaveBeenCalledWith({...STORED, embeddingModel: 'text-embedding-3-large'}),
    )
    expect(await screen.findByText('Embedding settings saved')).toBeInTheDocument()
  })

  it('saves nothing when nothing changed', async () => {
    renderCard()
    await screen.findByLabelText('Embedding model')
    fireEvent.click(screen.getByRole('button', {name: 'Save embeddings'}))
    expect(updateDeploymentAISettings).not.toHaveBeenCalled()
  })

  // The answer to the PUT is what the server now stores: a refetch could land on a replica whose settings cache
  // has not yet seen the write and would reset the form to the old values.
  it('takes the stored values from the save response, not a refetch', async () => {
    renderCard()
    const model = (await screen.findByLabelText('Embedding model')) as HTMLInputElement
    fireEvent.change(model, {target: {value: 'text-embedding-3-large'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save embeddings'}))

    expect(await screen.findByText('Embedding settings saved')).toBeInTheDocument()
    expect(model.value).toBe('text-embedding-3-large')
    expect(getDeploymentAISettings).toHaveBeenCalledTimes(1)
  })

  it('shows no budget or cost field', async () => {
    renderCard()
    await screen.findByLabelText('Embedding model')
    expect(screen.queryByText(/budget|USD|\$/i)).not.toBeInTheDocument()
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
    updateDeploymentAISettings.mockRejectedValueOnce(new Error('ai.embeddingModel is 300 characters, at most 200'))
    renderCard()
    const model = await screen.findByLabelText('Embedding model')
    fireEvent.change(model, {target: {value: 'other-model'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save embeddings'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('at most 200')
  })
})
