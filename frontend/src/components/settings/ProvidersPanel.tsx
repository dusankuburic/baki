import {useState, useEffect, useCallback} from 'react'
import ApiKeyInput from './ApiKeyInput'
import GitHubLoginButton from '@/components/chat/GitHubLoginButton'
import CopilotLoginButton from '@/components/chat/CopilotLoginButton'
import {providersApi} from '@/api'

interface ProviderEntry {
  id: string
  name: string
  configured: boolean
  authType: string
}

export default function ProvidersPanel() {
  const [_providers, setProviders] = useState<ProviderEntry[]>([])
  const [githubUser, setGithubUser] = useState<string | null>(null)
  const [copilotUser, setCopilotUser] = useState<string | null>(null)

  const refresh = useCallback(() => {
    providersApi.listProviders().then((ps: any) => {
      setProviders((ps || []).map((p: any) => ({
        id: p.id || '',
        name: p.name || '',
        configured: !!p.configured,
        authType: p.authType || '',
      })))
    }).catch(() => {})
    providersApi.getGitHubUser().then((u: any) => setGithubUser(u?.login || null)).catch(() => setGithubUser(null))
    providersApi.getCopilotUser().then((u: any) => setCopilotUser(u?.login || null)).catch(() => setCopilotUser(null))
  }, [])

  useEffect(() => { refresh() }, [refresh])

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">AI Providers</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Configure API keys for AI-powered analysis and chat.
      </p>

      <div className="space-y-8">
        <ProviderSection name="Claude" color="#d4a574">
          <ApiKeyInput provider="claude" label="Anthropic API Key" onConfigured={refresh} />
        </ProviderSection>

        <ProviderSection name="OpenAI" color="#10a37f">
          <ApiKeyInput provider="openai" label="OpenAI API Key" onConfigured={refresh} />
        </ProviderSection>

        <ProviderSection name="Gemini" color="#4285f4">
          <ApiKeyInput provider="gemini" label="Google AI API Key" onConfigured={refresh} />
        </ProviderSection>

        <ProviderSection name="xAI (Grok)" color="#f43f5e">
          <ApiKeyInput provider="xai" label="xAI API Key" onConfigured={refresh} />
        </ProviderSection>

        <ProviderSection name="GLM (z.ai)" color="#06b6d4">
          <ApiKeyInput provider="glm" label="z.ai API Key" onConfigured={refresh} />
        </ProviderSection>

        <ProviderSection name="GitHub Models" color="#8b5cf6">
          <div className="space-y-2">
            {githubUser ? (
              <div className="flex items-center gap-3">
                <span className="text-sm text-text-secondary">Connected as <strong className="text-text-primary">@{githubUser}</strong></span>
                <button
                  className="text-xs text-red-400 hover:text-red-300"
                  onClick={() => providersApi.revokeGitHubAuth().then(() => refresh()).catch(() => {})}
                >
                  Disconnect
                </button>
              </div>
            ) : (
              <GitHubLoginButton />
            )}
          </div>
        </ProviderSection>

        <ProviderSection name="GitHub Copilot" color="#6e40c9">
          <div className="space-y-2.5">
            <p className="text-xs text-text-tertiary leading-relaxed">
              Requires a GitHub account with an active{' '}
              <a href="https://github.com/features/copilot" target="_blank" rel="noopener noreferrer" className="text-brand-400 hover:underline">Copilot subscription</a>.
            </p>
            {copilotUser ? (
              <div className="flex items-center gap-3">
                <span className="text-sm text-text-secondary">Connected as <strong className="text-text-primary">@{copilotUser}</strong></span>
                <button
                  className="text-xs text-red-400 hover:text-red-300"
                  onClick={() => providersApi.revokeCopilotAuth().then(() => refresh()).catch(() => {})}
                >
                  Disconnect
                </button>
              </div>
            ) : (
              <CopilotLoginButton onAuthComplete={refresh} />
            )}
            {!copilotUser && (
              <details className="text-xs text-text-tertiary">
                <summary className="cursor-pointer hover:text-text-secondary transition-colors">
                  Or use a Personal Access Token
                </summary>
                <div className="mt-2">
                  <ApiKeyInput provider="copilot" label="GitHub Token (PAT)" onConfigured={refresh} />
                </div>
              </details>
            )}
          </div>
        </ProviderSection>
      </div>
    </div>
  )
}

function ProviderSection({name, color, children}: {name: string; color: string; children: React.ReactNode}) {
  return (
    <div>
      <div className="flex items-center gap-2 mb-3">
        <span className="w-3 h-3 rounded-full" style={{backgroundColor: color}} />
        <span className="text-base font-medium text-text-primary">{name}</span>
      </div>
      {children}
    </div>
  )
}
