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

// The model names the panel renders: the header picker's list, a listed
// model's display name, the purpose labels and the export naming. All derive
// from the providers listing (useProviderSetup).
export function useModelNames({providers}: ModelNamesDeps) {
  const {t} = useTranslation('chat')
  const {t: tc} = useTranslation('common')

  const labelOf: PurposeLabelOf = useCallback(p => tc(purposeLabelKey(p)), [tc])
  // A listed model's purpose label; none for a catalogue model.
  const purposeOf = useCallback(
    (m: ModelDetail | undefined) => (m && isRegistryEntry(m) ? purposeLabel(m, labelOf) : undefined),
    [labelOf],
  )
  const providerName = useCallback((id: string) => providers.find(p => p.id === id)?.name || id, [providers])
  const listedModels = useCallback(
    (providerId: string | undefined) => providers.find(p => p.id === providerId)?.models ?? EMPTY_MODELS,
    [providers],
  )

  // The header picker's list: the configured providers and their models.
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

  // The listing's name for a message's model, for a message that kept no
  // label of its own (one that is only an id counts as none).
  const listedModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (m.role !== 'assistant' || !m.model || storableModelLabel(m.modelLabel, m.model)) return undefined
      const entry = findModel(m.model, listedModels(m.provider))
      return entry ? entry.displayName || entry.id : undefined
    },
    [listedModels],
  )
  // An exported turn names its model the same way ("removed model" for a
  // registry id nothing names any more), never by its "m/<id>".
  const exportModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (!m.model) return undefined
      return modelNameOf(m.model, m.modelLabel, listedModels(m.provider)) ?? t('message.removedModel')
    },
    [listedModels, t],
  )

  return {labelOf, purposeOf, providerName, listedModels, pickerProviders, listedModelName, exportModelName}
}

// openAzureAccounts opens the settings' Azure AI Accounts section (composer
// guidance action and the notice's "Manage").
export function openAzureAccounts() {
  useUIStore.getState().openSettings('accounts', AZURE_ACCOUNTS_ANCHOR)
}
