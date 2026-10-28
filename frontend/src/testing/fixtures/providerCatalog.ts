
export interface CatalogFieldFixture {
  name: string
  label: string
  type: string
  required: boolean
  placeholder?: string
  help?: string
  options?: {value: string; label?: string}[]
}

export interface CatalogSpecFixture {
  id: string
  name: string
  authType: string
  modes: string[]
  caps: string[]
  credential: {kind: string; fields: CatalogFieldFixture[]; helpUrl?: string}
  endpoints: {save?: string; test?: string; start?: string; poll?: string; revoke?: string; user?: string}
  configured: boolean
  credentialStatus?: string
  orgCredentialStatus?: string
  color?: string
  icon?: string
}

export const API_KEY_FIELD: CatalogFieldFixture = {
  name: 'key',
  label: 'API key',
  type: 'secret',
  required: true,
  placeholder: 'sk-…',
}

export function providerSpec(over: Partial<CatalogSpecFixture> & {id: string}): CatalogSpecFixture {
  return {
    name: over.id,
    authType: 'api_key',
    modes: ['cloud', 'desktop'],
    caps: ['chat'],
    credential: {kind: 'api_key', fields: [API_KEY_FIELD]},
    endpoints: {save: '/api/keys/save', test: '/api/providers/test'},
    configured: false,
    ...over,
  }
}

export function deviceFlowSpec(id: string, over: Partial<CatalogSpecFixture> = {}): CatalogSpecFixture {
  return providerSpec({
    id,
    authType: 'oauth',
    credential: {kind: 'oauth_device', fields: []},
    endpoints: {
      start: `/api/providers/${id}/start`,
      poll: `/api/providers/${id}/poll`,
      revoke: `/api/providers/${id}/revoke`,
      user: `/api/providers/${id}/user`,
      test: '/api/providers/test',
    },
    ...over,
  })
}

export function mockProviderCatalog(): {providers: CatalogSpecFixture[]} {
  return {
    providers: [
      providerSpec({id: 'claude', name: 'Claude', color: '#d4a574', icon: 'cloud'}),
      providerSpec({
        id: 'mistral',
        name: 'Mistral',
        configured: true,
        credential: {kind: 'document', fields: [], helpUrl: 'https://docs.example.com/mistral'},
        endpoints: {test: '/api/providers/test'},
      }),
      providerSpec({
        id: 'quantum',
        name: 'Quantum AI',
        credential: {
          kind: 'api_key',
          fields: [{name: 'retina', label: 'Retina scan', type: 'biometric', required: true}],
        },
      }),
    ],
  }
}
