import {useCallback, useEffect, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Coins, RefreshCw, Save} from 'lucide-react'
import {adminApi, type DeploymentAISettings} from '@/api/admin'
import {providersApi} from '@/api'
import {Button, FormField, Input, NumberField, Select, useToast} from '@/components/shared'
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
  return (
    a.dailyBudget === b.dailyBudget &&
    a.perUserDailyBudget === b.perUserDailyBudget &&
    a.embeddingProvider === b.embeddingProvider &&
    a.embeddingModel === b.embeddingModel
  )
}

// The deployment-wide ai values: the budgets the server enforces on every replica and the embedding choice every
// knowledge base shares. In cloud a member's Settings write lands in their own document, so this is the only
// place these move.
export function AILimitsCard({visible = true, refreshKey = 0}: Props) {
  const {t} = useTranslation('admin')
  const toast = useToast()
  const {
    data: enforced,
    isLoading,
    error,
    setData,
    refetch,
  } = useAsync<DeploymentAISettings | null>(
    signal => (visible ? adminApi.getDeploymentAISettings(signal) : Promise.resolve(null)),
    [visible, refreshKey],
  )
  // The form starts from each load's enforced values (a derived reset, as NumberField keeps its draft).
  const [form, setForm] = useState<DeploymentAISettings | null>(null)
  const [loaded, setLoaded] = useState<DeploymentAISettings | null>(null)
  if (enforced && enforced !== loaded) {
    setLoaded(enforced)
    setForm(enforced)
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
  const dirty = !!form && !!enforced && !same(form, enforced)
  const patch = (p: Partial<DeploymentAISettings>) => {
    setSaveError(null)
    setForm(prev => (prev ? {...prev, ...p} : prev))
  }
  // Save is not gated on dirty: the budget fields commit on blur, and a click on a disabled button does not blur
  // the field in every browser, so the first click after typing would have saved nothing. A clean save is a no-op.
  const save = useCallback(async () => {
    if (!form || !dirty) return
    setSaving(true)
    setSaveError(null)
    try {
      // The answer is what the server now enforces. A refetch could land on a replica whose settings cache has
      // not yet seen the write and would reset the form to the old values.
      setData(await adminApi.updateDeploymentAISettings(form))
      toast.success(t('aiLimits.saved'))
    } catch (e) {
      setSaveError(getErrorMessage(e, t('aiLimits.saveFailed')))
    } finally {
      setSaving(false)
    }
  }, [form, dirty, setData, toast, t])

  return (
    <SectionCard
      id="admin-ai-limits"
      icon={Coins}
      title={t('aiLimits.title')}
      description={t('aiLimits.subtitle')}
      action={
        <Button variant="primary" size="sm" icon={Save} onClick={() => void save()} disabled={!form || saving}>
          {saving ? t('aiLimits.saving') : t('aiLimits.save')}
        </Button>
      }
    >
      {error && (
        <SectionNotice
          message={t('aiLimits.loadFailed')}
          action={
            <Button variant="ghost" size="sm" icon={RefreshCw} onClick={refetch} disabled={isLoading}>
              {t('aiLimits.retry')}
            </Button>
          }
        />
      )}
      {catalogFailed && (
        <SectionNotice
          message={t('aiLimits.catalogFailed')}
          action={
            <Button variant="ghost" size="sm" icon={RefreshCw} onClick={retryCatalog}>
              {t('aiLimits.retry')}
            </Button>
          }
        />
      )}
      {saveError && <SectionNotice message={saveError} />}
      {form && (
        <div className="grid gap-4 sm:grid-cols-2" data-testid="ai-limits-form">
          <FormField id="ai-limits-daily-budget" label={t('aiLimits.dailyBudget')}>
            <NumberField
              id="ai-limits-daily-budget"
              value={form.dailyBudget}
              onCommit={n => patch({dailyBudget: n})}
              min={0}
              step="0.01"
              integer={false}
              fallback={0}
              hint={t('aiLimits.dailyBudgetHint')}
            />
          </FormField>
          <FormField id="ai-limits-per-user-budget" label={t('aiLimits.perUserDailyBudget')}>
            <NumberField
              id="ai-limits-per-user-budget"
              value={form.perUserDailyBudget}
              onCommit={n => patch({perUserDailyBudget: n})}
              min={0}
              step="0.01"
              integer={false}
              fallback={0}
              hint={t('aiLimits.perUserDailyBudgetHint')}
            />
          </FormField>
          <FormField id="ai-limits-embedding-provider" label={t('aiLimits.embeddingProvider')}>
            <Select
              id="ai-limits-embedding-provider"
              value={form.embeddingProvider}
              onChange={e => patch({embeddingProvider: e.target.value})}
              options={[{value: '', label: t('aiLimits.embeddingProviderDefault')}, ...embeddingOptions]}
              // Until the catalog is in, the list is only the saved id: a choice from it could clear the provider.
              disabled={!catalog}
              hint={t('aiLimits.embeddingProviderHint')}
            />
          </FormField>
          <FormField id="ai-limits-embedding-model" label={t('aiLimits.embeddingModel')}>
            <Input
              id="ai-limits-embedding-model"
              value={form.embeddingModel}
              onChange={e => patch({embeddingModel: e.target.value})}
              maxLength={EMBEDDING_MODEL_MAX_LENGTH}
              placeholder={t('aiLimits.embeddingModelPlaceholder')}
              hint={t('aiLimits.embeddingModelHint')}
            />
          </FormField>
        </div>
      )}
    </SectionCard>
  )
}
