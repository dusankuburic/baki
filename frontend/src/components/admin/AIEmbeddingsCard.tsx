import {useCallback, useEffect, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Database, RefreshCw, Save} from 'lucide-react'
import {adminApi, type DeploymentAISettings} from '@/api/admin'
import {providersApi} from '@/api'
import {Button, FormField, Input, Select, useToast} from '@/components/shared'
import {SectionCard, SectionNotice} from '@/components/auth/SectionCard'
import {useAsync} from '@/hooks/useAsync'
import {EMBEDDING_MODEL_MAX_LENGTH} from '@/lib/constraints'
import {getErrorMessage} from '@/lib/getErrorMessage'
import type {ProviderSpec} from '@/types'
import {PROVIDER_CAP_EMBEDDINGS, PROVIDER_CAP_MODEL_ALIASES} from '@/types/providers'

interface Props {
  visible?: boolean
  refreshKey?: number
}

function same(a: DeploymentAISettings, b: DeploymentAISettings): boolean {
  return a.embeddingProvider === b.embeddingProvider && a.embeddingModel === b.embeddingModel
}

// The deployment-wide ai values: the embedding choice every knowledge base shares. In cloud a member's Settings
// write lands in their own document, so this is the only place these move.
export function AIEmbeddingsCard({visible = true, refreshKey = 0}: Props) {
  const {t} = useTranslation('admin')
  const toast = useToast()
  const {
    data: stored,
    isLoading,
    error,
    setData,
    refetch,
  } = useAsync<DeploymentAISettings | null>(
    signal => (visible ? adminApi.getDeploymentAISettings(signal) : Promise.resolve(null)),
    [visible, refreshKey],
  )
  // The form starts from each load's stored values (a derived reset).
  const [form, setForm] = useState<DeploymentAISettings | null>(null)
  const [loaded, setLoaded] = useState<DeploymentAISettings | null>(null)
  if (stored && stored !== loaded) {
    setLoaded(stored)
    setForm(stored)
  }

  const [catalog, setCatalog] = useState<ProviderSpec[] | null>(null)
  const [catalogFailed, setCatalogFailed] = useState(false)
  const [catalogAttempt, setCatalogAttempt] = useState(0)
  useEffect(() => {
    let cancelled = false
    providersApi
      .getProviderCatalog()
      .then(specs => {
        if (!cancelled) setCatalog(specs)
      })
      .catch(() => {
        if (!cancelled) setCatalogFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [catalogAttempt])
  const retryCatalog = () => {
    setCatalogFailed(false)
    setCatalogAttempt(n => n + 1)
  }
  const embeddingOptions = useMemo(() => {
    // A provider whose model ids are deployment aliases (Azure AI Foundry) embeds only on the operator's
    // PAD_EMBEDDING_PROVIDER pin, which this write cannot set; the server refuses it.
    const options = (catalog ?? [])
      .filter(p => p.caps.includes(PROVIDER_CAP_EMBEDDINGS) && !p.caps.includes(PROVIDER_CAP_MODEL_ALIASES))
      .map(p => ({value: p.id, label: p.name || p.id}))
    const saved = form?.embeddingProvider
    if (saved && !options.some(o => o.value === saved)) options.push({value: saved, label: saved})
    return options
  }, [catalog, form?.embeddingProvider])

  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const dirty = !!form && !!stored && !same(form, stored)
  const patch = (p: Partial<DeploymentAISettings>) => {
    setSaveError(null)
    setForm(prev => (prev ? {...prev, ...p} : prev))
  }
  const save = useCallback(async () => {
    if (!form || !dirty) return
    setSaving(true)
    setSaveError(null)
    try {
      // The answer is what the server now stores. A refetch could land on a replica whose settings cache has not
      // yet seen the write and would reset the form to the old values.
      setData(await adminApi.updateDeploymentAISettings(form))
      toast.success(t('aiEmbeddings.saved'))
    } catch (e) {
      setSaveError(getErrorMessage(e, t('aiEmbeddings.saveFailed')))
    } finally {
      setSaving(false)
    }
  }, [form, dirty, setData, toast, t])

  return (
    <SectionCard
      id="admin-ai-embeddings"
      icon={Database}
      title={t('aiEmbeddings.title')}
      description={t('aiEmbeddings.subtitle')}
      action={
        <Button variant="primary" size="sm" icon={Save} onClick={() => void save()} disabled={!form || saving}>
          {saving ? t('aiEmbeddings.saving') : t('aiEmbeddings.save')}
        </Button>
      }
    >
      {error && (
        <SectionNotice
          message={t('aiEmbeddings.loadFailed')}
          action={
            <Button variant="ghost" size="sm" icon={RefreshCw} onClick={refetch} disabled={isLoading}>
              {t('aiEmbeddings.retry')}
            </Button>
          }
        />
      )}
      {catalogFailed && (
        <SectionNotice
          message={t('aiEmbeddings.catalogFailed')}
          action={
            <Button variant="ghost" size="sm" icon={RefreshCw} onClick={retryCatalog}>
              {t('aiEmbeddings.retry')}
            </Button>
          }
        />
      )}
      {saveError && <SectionNotice message={saveError} />}
      {form && (
        <div className="grid gap-4 sm:grid-cols-2" data-testid="ai-embeddings-form">
          <FormField id="ai-embeddings-provider" label={t('aiEmbeddings.embeddingProvider')}>
            <Select
              id="ai-embeddings-provider"
              value={form.embeddingProvider}
              onChange={e => patch({embeddingProvider: e.target.value})}
              options={[{value: '', label: t('aiEmbeddings.embeddingProviderDefault')}, ...embeddingOptions]}
              // Until the catalog is in, the list is only the saved id: a choice from it could clear the provider.
              disabled={!catalog}
              hint={t('aiEmbeddings.embeddingProviderHint')}
            />
          </FormField>
          <FormField id="ai-embeddings-model" label={t('aiEmbeddings.embeddingModel')}>
            <Input
              id="ai-embeddings-model"
              value={form.embeddingModel}
              onChange={e => patch({embeddingModel: e.target.value})}
              maxLength={EMBEDDING_MODEL_MAX_LENGTH}
              placeholder={t('aiEmbeddings.embeddingModelPlaceholder')}
              hint={t('aiEmbeddings.embeddingModelHint')}
            />
          </FormField>
        </div>
      )}
    </SectionCard>
  )
}
