import {useCallback, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {findModel, modelNameOf, storableModelLabel} from '@/lib/modelIds'
import {isRegistryEntry} from '@/lib/modelGroups'
import {purposeLabel, purposeLabelKey, type PurposeLabelOf} from '@/lib/modelPurpose'
import {useUIStore} from '@/stores/uiStore'
import type {useProviderSetup} from '../hooks/useProviderSetup'
import {AZURE_ACCOUNTS_ANCHOR, type PickerProvider} from '../ModelPicker'
import type {ChatMessage, ModelDetail} from '@/types'

type ProviderSetup = ReturnType<typeof useProviderSetup>

const EMPTY_MODELS: ModelDetail[] = []

interface ModelNamesDeps {
  providers: ProviderSetup['providers']
}

export function useModelNames({providers}: ModelNamesDeps) {
  const {t} = useTranslation('chat')
  const {t: tc} = useTranslation('common')

  const labelOf: PurposeLabelOf = useCallback(p => tc(purposeLabelKey(p)), [tc])
  const purposeOf = useCallback(
    (m: ModelDetail | undefined) => (m && isRegistryEntry(m) ? purposeLabel(m, labelOf) : undefined),
    [labelOf],
  )
  const providerName = useCallback((id: string) => providers.find(p => p.id === id)?.name || id, [providers])
  const listedModels = useCallback(
    (providerId: string | undefined) => providers.find(p => p.id === providerId)?.models ?? EMPTY_MODELS,
    [providers],
  )

  const pickerProviders: PickerProvider[] = useMemo(
    () =>
      providers
        .filter(p => p.configured)
        .map(p => ({
          id: p.id,
          name: p.name,
          models: p.models,
          ...(p.shared ? {shared: true} : {}),
          ...(p.icon ? {icon: p.icon} : {}),
        })),
    [providers],
  )

  const listedModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (m.role !== 'assistant' || !m.model || storableModelLabel(m.modelLabel, m.model)) return undefined
      const entry = findModel(m.model, listedModels(m.provider))
      return entry ? entry.displayName || entry.id : undefined
    },
    [listedModels],
  )
  const exportModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (!m.model) return undefined
      return modelNameOf(m.model, m.modelLabel, listedModels(m.provider)) ?? t('message.removedModel')
    },
    [listedModels, t],
  )

  return {labelOf, purposeOf, providerName, listedModels, pickerProviders, listedModelName, exportModelName}
}

export function openAzureAccounts() {
  useUIStore.getState().openSettings('accounts', AZURE_ACCOUNTS_ANCHOR)
}
