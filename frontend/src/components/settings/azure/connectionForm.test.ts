import {describe, it, expect} from 'vitest'
import type {AzureConnection} from '@/types'
import {
  EMPTY_CONNECTION_FORM,
  connectionErrorTarget,
  connectionFormFromConnection,
  endpointErrorKey,
  formAuthMode,
  formsDiffer,
  guidErrorKey,
  nameFromEndpoint,
  secretCarry,
  secretField,
  secretHostsKept,
  secretValue,
  tenantErrorKey,
  toConnectionInput,
  toConnectionUpdateInput,
  validateConnectionForm,
  withEndpoint,
  withName,
  type ConnectionFormState,
} from './connectionForm'

const GUID = '123e4567-e89b-42d3-a456-426614174000'
const GUID2 = '223e4567-e89b-42d3-a456-426614174000'

function connection(
  config: Partial<AzureConnection['config']> = {},
  over: Partial<AzureConnection> = {},
): AzureConnection {
  return {
    id: 'abc0123456',
    name: 'contoso-prod',
    shared: false,
    legacy: false,
    config: {endpoint: 'https://contoso.openai.azure.com', authMode: 'api_key', hasSecret: true, ...config},
    status: 'ready',
    canDiscover: false,
    supportsAgents: false,
    suggestDiscovery: false,
    modelCount: 0,
    updatedAt: '2026-09-30T10:00:00.000001Z',
    ...over,
  }
}

const form = (over: Partial<ConnectionFormState> = {}): ConnectionFormState => ({
  ...EMPTY_CONNECTION_FORM,
  name: 'contoso-prod',
  nameEdited: true,
  endpoint: 'https://contoso.openai.azure.com',
  ...over,
})

describe('field validators (moved from AzureFoundrySection)', () => {
  it('checks the endpoint: absolute https URL on an Azure AI Foundry host', () => {
    expect(endpointErrorKey('')).toBeNull()
    expect(endpointErrorKey('not a url')).toBe('providers.azureEndpointInvalid')
    expect(endpointErrorKey('http://x.openai.azure.com')).toBe('providers.azureEndpointHttps')
    expect(endpointErrorKey('https://example.com')).toBe('providers.azureHostRejected')
    // A bare suffix is no resource host.
    expect(endpointErrorKey('https://.openai.azure.com')).toBe('providers.azureHostRejected')
    expect(endpointErrorKey(' https://my-res.services.ai.azure.com/ ')).toBeNull()
    expect(endpointErrorKey('https://my-res.cognitiveservices.azure.com')).toBeNull()
  })

  it('checks client and tenant ids', () => {
    expect(guidErrorKey('')).toBeNull()
    expect(guidErrorKey(GUID)).toBeNull()
    expect(guidErrorKey('nope')).toBe('providers.azureGuidInvalid')
    expect(tenantErrorKey('')).toBeNull()
    expect(tenantErrorKey(GUID)).toBeNull()
    expect(tenantErrorKey('contoso.onmicrosoft.com')).toBeNull()
    expect(tenantErrorKey('common')).toBe('providers.azureTenantInvalid')
    expect(tenantErrorKey('10.0.0.1')).toBe('providers.azureTenantInvalid')
    expect(tenantErrorKey(`${'a'.repeat(250)}.com`)).toBe('providers.azureTenantInvalid')
  })

  it('carries a secret only across the same hosts (F-34)', () => {
    const prev = {
      endpoint: 'https://contoso.openai.azure.com',
      projectEndpoint: 'https://proj.services.ai.azure.com/api/projects/p',
    }
    expect(secretHostsKept(prev, {endpoint: 'https://CONTOSO.openai.azure.com/'})).toBe(true)
    expect(secretHostsKept(prev, {endpoint: 'https://other.openai.azure.com'})).toBe(false)
    expect(secretHostsKept(prev, {endpoint: ''})).toBe(false)
    expect(secretHostsKept(prev, {...prev, projectEndpoint: 'https://proj.services.ai.azure.com/api/projects/q'})).toBe(
      true,
    )
    expect(secretHostsKept(prev, {...prev, projectEndpoint: 'https://contoso.openai.azure.com/api/projects/q'})).toBe(
      true,
    )
    expect(secretHostsKept(prev, {...prev, projectEndpoint: 'https://evil.services.ai.azure.com/api/projects/p'})).toBe(
      false,
    )
    expect(secretHostsKept(prev, {...prev, projectEndpoint: 'garbage'})).toBe(false)
    expect(secretHostsKept({endpoint: prev.endpoint}, {...prev})).toBe(false)
  })
})

describe('the form', () => {
  it('fills the name from the host until the user edits it', () => {
    expect(nameFromEndpoint('https://my-res.openai.azure.com/')).toBe('my-res')
    expect(nameFromEndpoint('my-res.openai.azure.com')).toBe('')
    expect(nameFromEndpoint(`https://${'a'.repeat(63)}b.openai.azure.com`)).toHaveLength(64)

    const fresh = withEndpoint(EMPTY_CONNECTION_FORM, 'https://my-res.openai.azure.com')
    expect(fresh).toMatchObject({name: 'my-res', nameEdited: false})
    const named = withName(fresh, 'Production')
    expect(withEndpoint(named, 'https://other.openai.azure.com')).toMatchObject({name: 'Production', nameEdited: true})
  })

  it('edits a saved resource with its public settings and blank secrets', () => {
    const conn = connection({
      authMode: 'entra',
      tenantId: GUID,
      clientId: GUID2,
      projectEndpoint: 'https://p.services.ai.azure.com/api/projects/x',
    })
    expect(connectionFormFromConnection(conn)).toEqual({
      name: 'contoso-prod',
      nameEdited: true,
      endpoint: 'https://contoso.openai.azure.com',
      authMode: 'entra',
      apiKey: '',
      tenantId: GUID,
      clientId: GUID2,
      clientSecret: '',
      projectEndpoint: 'https://p.services.ai.azure.com/api/projects/x',
    })
    expect(connectionFormFromConnection(connection())).toMatchObject({tenantId: '', clientId: '', projectEndpoint: ''})
  })

  // U1: without a readable credential document (a lender's credential
  // disabled, an unreadable one) the server sends blank settings with
  // authMode ''. The form still starts on a real sign-in mode, so one is
  // selected and its fields are the ones validated and sent.
  it('starts a resource whose settings came back blank on a real sign-in mode', () => {
    const blank = connection({endpoint: '', authMode: '', hasSecret: false}, {status: 'unreadable'})
    const f = connectionFormFromConnection(blank)
    expect(f).toMatchObject({endpoint: '', authMode: 'api_key', tenantId: '', clientId: ''})
    expect(formAuthMode(blank.config)).toBe('api_key')
    // What the API-key field shows is what is validated and sent.
    expect(validateConnectionForm(f, {connection: blank}).errors).toEqual({endpoint: 'required', secret: 'required'})
    const filled = {...f, endpoint: 'https://my-res.openai.azure.com', apiKey: ' sk-new '}
    expect(validateConnectionForm(filled, {connection: blank}).firstInvalid).toBeNull()
    expect(toConnectionUpdateInput(filled, f, blank)).toEqual({
      config: {endpoint: 'https://my-res.openai.azure.com', auth: {mode: 'api_key', key: 'sk-new'}},
      updatedAt: blank.updatedAt,
    })

    // A mode this client doesn't know: Entra ID when a principal is known.
    const unknown = (config: Partial<AzureConnection['config']>) =>
      formAuthMode({endpoint: 'https://x.openai.azure.com', hasSecret: true, ...config, authMode: 'managed' as never})
    expect(unknown({tenantId: GUID})).toBe('entra')
    expect(unknown({clientId: GUID2})).toBe('entra')
    expect(unknown({})).toBe('api_key')
    expect(formAuthMode({endpoint: '', authMode: '', hasSecret: false, tenantId: ' '})).toBe('api_key')
    // The known modes stay as they are.
    expect(formAuthMode(connection({authMode: 'entra_cli'}).config)).toBe('entra_cli')
  })

  it('is dirty only when a value changed', () => {
    const base = form()
    expect(formsDiffer(base, {...base})).toBe(false)
    expect(formsDiffer(base, {...base, nameEdited: false})).toBe(false)
    expect(formsDiffer(base, {...base, apiKey: 'k'})).toBe(true)
    expect(formsDiffer(base, {...base, authMode: 'entra'})).toBe(true)
  })

  it("reads the auth mode's secret", () => {
    expect(secretValue(form({apiKey: 'k', clientSecret: 's'}))).toBe('k')
    expect(secretValue(form({authMode: 'entra', apiKey: 'k', clientSecret: 's'}))).toBe('s')
    expect(secretValue(form({authMode: 'entra_cli', apiKey: 'k'}))).toBe('')
    // The field the modal renders for each mode.
    expect(secretField('api_key')).toBe('apiKey')
    expect(secretField('entra')).toBe('clientSecret')
    expect(secretField('entra_cli')).toBeNull()
  })
})

describe('secretCarry', () => {
  const entraConn = connection({authMode: 'entra', tenantId: GUID, clientId: GUID2})
  const entraForm = (over: Partial<ConnectionFormState> = {}) =>
    form({authMode: 'entra', tenantId: GUID, clientId: GUID2, ...over})

  it('needs no secret for the Azure CLI', () => {
    expect(secretCarry(entraConn, form({authMode: 'entra_cli'}))).toBe('none')
  })

  it('requires one for a new resource, nothing stored, or another auth mode', () => {
    expect(secretCarry(null, form())).toBe('required')
    expect(secretCarry(undefined, form())).toBe('required')
    expect(secretCarry(connection({hasSecret: false}), form())).toBe('required')
    expect(secretCarry(connection(), form({authMode: 'entra'}))).toBe('required')
  })

  // "Enter credentials": the stored secret is unusable, so blank never keeps it.
  it('requires one when the resource needs its credentials entered again', () => {
    expect(secretCarry(connection({}, {status: 'needs_credentials'}), form())).toBe('required')
    expect(secretCarry(connection({}, {status: 'unreadable'}), form())).toBe('required')
    expect(secretCarry(connection({}, {status: 'invalid_config'}), form())).toBe('kept')
  })

  it('keeps it for the same mode, principal and hosts', () => {
    expect(secretCarry(connection(), form())).toBe('kept')
    // Tenant and client compare trimmed and case-insensitively.
    expect(secretCarry(entraConn, entraForm({tenantId: ` ${GUID.toUpperCase()} `}))).toBe('kept')
    // A stored principal without ids matches a form without them.
    expect(secretCarry(connection({authMode: 'entra'}), form({authMode: 'entra'}))).toBe('kept')
  })

  it('asks to re-enter it when the endpoint, project host or principal changed', () => {
    expect(secretCarry(connection(), form({endpoint: 'https://other.openai.azure.com'}))).toBe('reenter')
    expect(
      secretCarry(connection(), form({projectEndpoint: 'https://evil.services.ai.azure.com/api/projects/p'})),
    ).toBe('reenter')
    expect(secretCarry(entraConn, entraForm({clientId: GUID}))).toBe('reenter')
    expect(secretCarry(entraConn, entraForm({tenantId: 'contoso.onmicrosoft.com'}))).toBe('reenter')
  })
})

describe('validateConnectionForm', () => {
  it('passes a complete new API-key or Entra ID resource', () => {
    expect(validateConnectionForm(form({apiKey: 'k'}))).toEqual({errors: {}, firstInvalid: null, endpointWarning: null})
    expect(
      validateConnectionForm(
        form({authMode: 'entra', tenantId: 'contoso.onmicrosoft.com', clientId: GUID2, clientSecret: 's'}),
      ),
    ).toEqual({errors: {}, firstInvalid: null, endpointWarning: null})
  })

  it('reports required fields in tab order', () => {
    const v = validateConnectionForm({...EMPTY_CONNECTION_FORM, authMode: 'entra'})
    expect(v.errors).toEqual({
      endpoint: 'required',
      name: 'required',
      tenantId: 'required',
      clientId: 'required',
      secret: 'required',
    })
    expect(v.firstInvalid).toBe('endpoint')
  })

  it('reports format errors, and the host rule only as a warning', () => {
    expect(validateConnectionForm(form({endpoint: 'http://x.openai.azure.com', apiKey: 'k'})).errors).toEqual({
      endpoint: 'providers.azureEndpointHttps',
    })
    const warned = validateConnectionForm(form({endpoint: 'https://proxy.example.com', apiKey: 'k'}))
    expect(warned).toEqual({errors: {}, firstInvalid: null, endpointWarning: 'providers.azureHostRejected'})
    const entra = validateConnectionForm(
      form({authMode: 'entra', tenantId: 'common', clientId: 'x', clientSecret: 's'}),
    )
    expect(entra.errors).toEqual({tenantId: 'providers.azureTenantInvalid', clientId: 'providers.azureGuidInvalid'})
    expect(entra.firstInvalid).toBe('tenantId')
  })

  it('caps the name at 64 characters', () => {
    expect(validateConnectionForm(form({name: 'x'.repeat(64), apiKey: 'k'})).errors).toEqual({})
    expect(validateConnectionForm(form({name: 'x'.repeat(65), apiKey: 'k'})).errors).toEqual({name: 'tooLong'})
  })

  it('offers the Azure CLI only where allowed, with an optional tenant', () => {
    expect(validateConnectionForm(form({authMode: 'entra_cli'})).errors).toEqual({authMode: 'cliUnavailable'})
    expect(validateConnectionForm(form({authMode: 'entra_cli'}), {allowCli: true}).errors).toEqual({})
    expect(validateConnectionForm(form({authMode: 'entra_cli', tenantId: 'common'}), {allowCli: true}).errors).toEqual({
      tenantId: 'providers.azureTenantInvalid',
    })
  })

  it('lets a blank secret keep the stored one, but not after the carry was dropped', () => {
    const conn = connection()
    expect(validateConnectionForm(form(), {connection: conn}).errors).toEqual({})
    const moved = validateConnectionForm(form({endpoint: 'https://other.openai.azure.com'}), {connection: conn})
    expect(moved.errors).toEqual({secret: 'required'})
    expect(moved.firstInvalid).toBe('secret')
  })
})

describe('request bodies', () => {
  it('creates with trimmed values and leaves blank optionals and a blank secret out', () => {
    expect(toConnectionInput(form({name: ' prod ', endpoint: ' https://x.openai.azure.com ', apiKey: '  '}))).toEqual({
      name: 'prod',
      config: {endpoint: 'https://x.openai.azure.com', auth: {mode: 'api_key'}},
    })
    expect(
      toConnectionInput(form({apiKey: ' k ', projectEndpoint: ' https://p.services.ai.azure.com/api/projects/x '})),
    ).toEqual({
      name: 'contoso-prod',
      config: {
        endpoint: 'https://contoso.openai.azure.com',
        auth: {mode: 'api_key', key: 'k'},
        projectEndpoint: 'https://p.services.ai.azure.com/api/projects/x',
      },
    })
  })

  it('sends each auth mode in its own shape', () => {
    expect(
      toConnectionInput(form({authMode: 'entra', tenantId: ` ${GUID} `, clientId: GUID2, clientSecret: 's'})).config
        .auth,
    ).toEqual({
      mode: 'entra',
      tenantId: GUID,
      clientId: GUID2,
      clientSecret: 's',
    })
    expect(toConnectionInput(form({authMode: 'entra', tenantId: GUID, clientId: GUID2})).config.auth).toEqual({
      mode: 'entra',
      tenantId: GUID,
      clientId: GUID2,
    })
    expect(toConnectionInput(form({authMode: 'entra_cli', tenantId: GUID, clientId: 'ignored'})).config.auth).toEqual({
      mode: 'entra_cli',
      tenantId: GUID,
    })
    expect(toConnectionInput(form({authMode: 'entra_cli'})).config.auth).toEqual({mode: 'entra_cli'})
  })

  it('updates only what changed, with the compare-and-swap stamps', () => {
    const conn = connection({}, {credentialUpdatedAt: '2026-09-30T09:00:00.000001Z'})
    const base = connectionFormFromConnection(conn)

    expect(toConnectionUpdateInput(base, base, conn)).toEqual({updatedAt: conn.updatedAt})
    expect(toConnectionUpdateInput({...base, name: 'renamed'}, base, conn)).toEqual({
      name: 'renamed',
      updatedAt: conn.updatedAt,
    })
    expect(toConnectionUpdateInput({...base, apiKey: 'new-key'}, base, conn)).toEqual({
      config: {endpoint: conn.config.endpoint, auth: {mode: 'api_key', key: 'new-key'}},
      credentialUpdatedAt: conn.credentialUpdatedAt,
      updatedAt: conn.updatedAt,
    })
    // Whitespace alone changes nothing.
    expect(toConnectionUpdateInput({...base, endpoint: ` ${base.endpoint} `}, base, conn)).toEqual({
      updatedAt: conn.updatedAt,
    })
    const personal = connection({}, {updatedAt: ''})
    expect(toConnectionUpdateInput({...base, endpoint: 'https://y.openai.azure.com'}, base, personal)).toEqual({
      config: {endpoint: 'https://y.openai.azure.com', auth: {mode: 'api_key'}},
    })
  })
})

describe('connectionErrorTarget', () => {
  const err = (code: string, message = '', status = 400) => ({code, message, status})

  it('maps the settings codes to their fields', () => {
    expect(connectionErrorTarget(err('INVALID_ENDPOINT'))).toBe('endpoint')
    expect(connectionErrorTarget(err('HOST_NOT_ALLOWED'))).toBe('endpoint')
    expect(connectionErrorTarget(err('INVALID_PROJECT_ENDPOINT'))).toBe('projectEndpoint')
    expect(connectionErrorTarget(err('MISSING_SECRET'))).toBe('secret')
    expect(connectionErrorTarget(err('INVALID_CONNECTION_NAME'))).toBe('name')
    expect(connectionErrorTarget(err('DUPLICATE_CONNECTION_NAME', '', 409))).toBe('name')
  })

  it('routes INVALID_AUTH by the part of the credential its message names', () => {
    const auth = (message: string) => connectionErrorTarget(err('INVALID_AUTH', message))
    expect(auth('the tenant ID must be the directory (tenant) ID GUID or a domain name')).toBe('tenantId')
    expect(auth('the client ID must be a GUID')).toBe('clientId')
    expect(auth('the client secret is too long')).toBe('secret')
    expect(auth('the API key is too long')).toBe('secret')
    expect(auth('the Azure CLI sign-in mode takes no client ID or client secret')).toBe('authMode')
    expect(auth('unknown authentication mode "managed"')).toBe('authMode')
    expect(auth('something new')).toBe('form')
    expect(connectionErrorTarget({code: 'INVALID_AUTH'})).toBe('form')
  })

  it('prefers the field the server names in details.field', () => {
    const withField = (code: string, field: unknown, message = 'something new') =>
      connectionErrorTarget({code, message, status: 400, details: {field}})
    expect(withField('INVALID_AUTH', 'mode')).toBe('authMode')
    expect(withField('INVALID_AUTH', 'key')).toBe('secret')
    expect(withField('INVALID_AUTH', 'tenantId', 'the client ID must be a GUID')).toBe('tenantId')
    expect(withField('INVALID_AUTH', 'clientId')).toBe('clientId')
    expect(withField('INVALID_AUTH', 'clientSecret')).toBe('secret')
    expect(withField('MISSING_SECRET', 'mode')).toBe('authMode')
    expect(withField('MISSING_SECRET', 'clientSecret')).toBe('secret')
    // An unknown or malformed field falls back to the message (or the secret).
    expect(withField('INVALID_AUTH', 'toString', 'the client ID must be a GUID')).toBe('clientId')
    expect(withField('MISSING_SECRET', 42)).toBe('secret')
    expect(connectionErrorTarget({code: 'MISSING_SECRET', details: null})).toBe('secret')
  })

  it('sends the re-index, conflict, read-only and anything else to their places', () => {
    expect(connectionErrorTarget(err('REINDEX_REQUIRED', '', 409))).toBe('reindex')
    expect(connectionErrorTarget(err('CONFLICT', '', 409))).toBe('conflict')
    expect(connectionErrorTarget(err('FORBIDDEN', '', 403))).toBe('forbidden')
    expect(connectionErrorTarget(err('TOO_MANY_CONNECTIONS', '', 409))).toBe('form')
    expect(connectionErrorTarget(new TypeError('Failed to fetch'))).toBe('form')
    expect(connectionErrorTarget(null)).toBe('form')
  })
})
