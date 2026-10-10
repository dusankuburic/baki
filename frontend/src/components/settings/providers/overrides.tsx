import {useMemo, useState, type ComponentType} from 'react'
import {useTranslation} from 'react-i18next'
import AzureFoundryPanel from '../azure/AzureFoundryPanel'
import {AZURE_SECTION_ANCHOR} from '../azure/registryView'
import LocalModelsSection from '../LocalModelsSection'
import CredentialForm from './CredentialForm'
import type {ProviderSectionProps} from './ProviderSection'
import type {AzureScope, GitHubUser, KnownProviderID, ProviderSpec} from '@/types'

const USER_SCOPE: AzureScope = {kind: 'user'}

// The providers whose section is not the generic form: Azure AI Foundry's
// registry (resources and models), the local model's base-URL form with its
// desktop gating, and Copilot's device flow with the PAT fallback. Every
// other catalogue entry, known or not, renders from its credential schema.

// Its heading is the "Manage models…" deep link's anchor
// (AZURE_SECTION_ANCHOR = provider-azure-foundry): the panel focuses it once
// its resources and models have loaded. The first chat model makes Azure the
// active provider only when the current one cannot chat.
function AzureOverride({onActivated}: ProviderSectionProps) {
  return (
    <AzureFoundryPanel
      scope={USER_SCOPE}
      headingLevel={4}
      anchorId={AZURE_SECTION_ANCHOR}
      onFirstChatModel={() => onActivated('azure-foundry', 'ifUnconfigured')}
    />
  )
}

// A cloud server answers LOCAL_MODELS_UNAVAILABLE to the section's own GET;
// it reports that and the host drops the heading with it.
function LocalOverride({spec, onConfigured, onUnavailable}: ProviderSectionProps) {
  return <LocalModelsSection onConfigured={onConfigured} onUnavailable={() => onUnavailable(spec.id)} />
}

function CopilotOverride({spec, onConfigured, onActivated}: ProviderSectionProps) {
  const {t} = useTranslation('settings')
  // The device flow's signed-in user, as its button learns it: the PAT
  // fallback is offered only while nobody is signed in.
  const [user, setUser] = useState<GitHubUser | null>(null)
  const patSpec = useMemo<ProviderSpec>(
    () => ({
      ...spec,
      credential: {
        kind: 'api_key',
        fields: [
          {
            name: 'key',
            label: t('providers.githubPat'),
            type: 'secret',
            required: true,
            placeholder: t('apiKey.placeholder'),
          },
        ],
      },
    }),
    [spec, t],
  )
  return (
    <div className="space-y-2.5">
      <p className="text-xs text-text-tertiary leading-relaxed">
        {t('providers.copilotRequires')}{' '}
        <a
          href="https://github.com/features/copilot"
          target="_blank"
          rel="noopener noreferrer"
          className="text-brand-400 hover:underline"
        >
          {t('providers.copilotSubscriptionLink')}
        </a>
        .
      </p>
      <CredentialForm
        spec={spec}
        onConfigured={onConfigured}
        onSignedIn={() => onActivated(spec.id, 'always')}
        onUser={setUser}
      />
      {!user && (
        <details className="text-xs text-text-tertiary">
          <summary className="cursor-pointer hover:text-text-secondary transition-colors">
            {t('providers.orUsePat')}
          </summary>
          <div className="mt-2">
            <CredentialForm spec={patSpec} onConfigured={onConfigured} />
          </div>
        </details>
      )}
    </div>
  )
}

export const PROVIDER_OVERRIDES: Partial<Record<KnownProviderID, ComponentType<ProviderSectionProps>>> = {
  'azure-foundry': AzureOverride,
  local: LocalOverride,
  copilot: CopilotOverride,
}
