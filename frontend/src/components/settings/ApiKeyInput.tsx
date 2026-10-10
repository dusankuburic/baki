import {useCallback, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import CredentialForm from './providers/CredentialForm'
import type {ProviderSpec} from '@/types'

interface Props {
  provider: string
  label: string
  onConfigured?: () => void
}

// The one-key form of the built-in sections (providers/LegacyProvidersPanel):
// the generic CredentialForm over an api_key entry the bundle writes itself,
// since a server without the catalogue sends none. Goes with the legacy panel.
export default function ApiKeyInput({provider, label, onConfigured}: Props) {
  const {t} = useTranslation('settings')
  const spec = useMemo<ProviderSpec>(
    () => ({
      id: provider,
      name: label,
      authType: 'api_key',
      modes: [],
      caps: [],
      credential: {
        kind: 'api_key',
        fields: [{name: 'key', label, type: 'secret', required: true, placeholder: t('apiKey.placeholder')}],
      },
      endpoints: {save: '/api/keys/save', test: '/api/providers/test'},
      configured: false,
    }),
    [provider, label, t],
  )
  const handleConfigured = useCallback(() => onConfigured?.(), [onConfigured])
  return <CredentialForm spec={spec} onConfigured={handleConfigured} />
}
