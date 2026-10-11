import type {
  AzureConnection,
  AzureConnectionConfigInput,
  AzureConnectionCreateInput,
  AzureConnectionPublicConfig,
  AzureConnectionUpdateInput,
  AzureFoundryAuthInput,
  AzureFoundryAuthMode,
} from '@/types'

const AZURE_HOST_SUFFIXES = ['.openai.azure.com', '.services.ai.azure.com', '.cognitiveservices.azure.com']

const GUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const TENANT_DOMAIN_RE = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/i

export type AzureFieldErrorKey =
  | 'providers.azureEndpointInvalid'
  | 'providers.azureEndpointHttps'
  | 'providers.azureHostRejected'
  | 'providers.azureGuidInvalid'
  | 'providers.azureTenantInvalid'

export function endpointErrorKey(value: string): AzureFieldErrorKey | null {
  const raw = value.trim()
  if (!raw) return null
  let url: URL | null = null
  try {
    url = new URL(raw)
  } catch {
  }
  if (!url || !url.hostname) return 'providers.azureEndpointInvalid'
  if (url.protocol !== 'https:') return 'providers.azureEndpointHttps'
  const host = url.hostname.toLowerCase()
  if (!AZURE_HOST_SUFFIXES.some(s => host.endsWith(s) && host.length > s.length)) return 'providers.azureHostRejected'
  return null
}

export function azureHost(raw: string): string {
  try {
    return new URL(raw.trim()).host.toLowerCase()
  } catch {
    return ''
  }
}

export function secretHostsKept(
  prev: {endpoint: string; projectEndpoint?: string},
  next: {endpoint: string; projectEndpoint?: string},
): boolean {
  const prevHost = azureHost(prev.endpoint)
  const nextHost = azureHost(next.endpoint)
  if (!nextHost || nextHost !== prevHost) return false
  const project = (next.projectEndpoint ?? '').trim()
  if (!project) return true
  const p = azureHost(project)
  return !!p && (p === prevHost || p === azureHost(prev.projectEndpoint ?? ''))
}

export function guidErrorKey(value: string): AzureFieldErrorKey | null {
  return value.trim() && !GUID_RE.test(value.trim()) ? 'providers.azureGuidInvalid' : null
}

export function tenantErrorKey(value: string): AzureFieldErrorKey | null {
  const v = value.trim()
  if (!v || GUID_RE.test(v)) return null
  const isIPv4 = /^\d{1,3}(\.\d{1,3}){3}$/.test(v)
  return v.length <= 253 && TENANT_DOMAIN_RE.test(v) && !isIPv4 ? null : 'providers.azureTenantInvalid'
}

export const CONNECTION_NAME_MAX_LENGTH = 64

export interface ConnectionFormState {
  name: string
  nameEdited: boolean
  endpoint: string
  authMode: AzureFoundryAuthMode
  apiKey: string
  tenantId: string
  clientId: string
  clientSecret: string
  projectEndpoint: string
}

export const EMPTY_CONNECTION_FORM: ConnectionFormState = {
  name: '',
  nameEdited: false,
  endpoint: '',
  authMode: 'api_key',
  apiKey: '',
  tenantId: '',
  clientId: '',
  clientSecret: '',
  projectEndpoint: '',
}

const VALUE_FIELDS = [
  'name',
  'endpoint',
  'authMode',
  'apiKey',
  'tenantId',
  'clientId',
  'clientSecret',
  'projectEndpoint',
] as const satisfies readonly (keyof ConnectionFormState)[]

const CONFIG_FIELDS = ['endpoint', 'authMode', 'tenantId', 'clientId', 'projectEndpoint'] as const

export function formAuthMode(config: AzureConnectionPublicConfig): AzureFoundryAuthMode {
  const mode = config.authMode
  if (mode === 'api_key' || mode === 'entra' || mode === 'entra_cli') return mode
  return config.tenantId?.trim() || config.clientId?.trim() ? 'entra' : 'api_key'
}

export function connectionFormFromConnection(conn: AzureConnection): ConnectionFormState {
  return {
    ...EMPTY_CONNECTION_FORM,
    name: conn.name,
    nameEdited: true,
    endpoint: conn.config.endpoint,
    authMode: formAuthMode(conn.config),
    tenantId: conn.config.tenantId ?? '',
    clientId: conn.config.clientId ?? '',
    projectEndpoint: conn.config.projectEndpoint ?? '',
  }
}

export function nameFromEndpoint(endpoint: string): string {
  let host: string
  try {
    host = new URL(endpoint.trim()).hostname
  } catch {
    return ''
  }
  return [...(host.split('.')[0] ?? '')].slice(0, CONNECTION_NAME_MAX_LENGTH).join('')
}

export function withEndpoint(form: ConnectionFormState, endpoint: string): ConnectionFormState {
  return form.nameEdited ? {...form, endpoint} : {...form, endpoint, name: nameFromEndpoint(endpoint)}
}

export function withName(form: ConnectionFormState, name: string): ConnectionFormState {
  return {...form, name, nameEdited: true}
}

export function formsDiffer(a: ConnectionFormState, b: ConnectionFormState): boolean {
  return VALUE_FIELDS.some(k => a[k] !== b[k])
}

export function secretField(mode: AzureFoundryAuthMode): 'apiKey' | 'clientSecret' | null {
  return mode === 'api_key' ? 'apiKey' : mode === 'entra_cli' ? null : 'clientSecret'
}

export function secretValue(form: ConnectionFormState): string {
  const field = secretField(form.authMode)
  return field ? form[field] : ''
}

export type SecretCarry = 'kept' | 'reenter' | 'required' | 'none'

export function secretCarry(conn: AzureConnection | null | undefined, form: ConnectionFormState): SecretCarry {
  if (!secretField(form.authMode)) return 'none'
  if (!conn || !conn.config.hasSecret || conn.config.authMode !== form.authMode) return 'required'
  if (conn.status === 'needs_credentials' || conn.status === 'unreadable') return 'required'
  const norm = (s?: string) => (s ?? '').trim().toLowerCase()
  const samePrincipal =
    form.authMode !== 'entra' ||
    (norm(form.tenantId) === norm(conn.config.tenantId) && norm(form.clientId) === norm(conn.config.clientId))
  return samePrincipal && secretHostsKept(conn.config, form) ? 'kept' : 'reenter'
}

export type ConnectionFormField =
  'endpoint' | 'name' | 'authMode' | 'tenantId' | 'clientId' | 'secret' | 'projectEndpoint'

export const CONNECTION_FIELD_ORDER: readonly ConnectionFormField[] = [
  'endpoint',
  'name',
  'authMode',
  'tenantId',
  'clientId',
  'secret',
  'projectEndpoint',
]

export type ConnectionFieldError = 'required' | 'tooLong' | 'cliUnavailable' | AzureFieldErrorKey

export interface ConnectionValidation {
  errors: Partial<Record<ConnectionFormField, ConnectionFieldError>>
  firstInvalid: ConnectionFormField | null
  endpointWarning: AzureFieldErrorKey | null
}

export function validateConnectionForm(
  form: ConnectionFormState,
  opts: {connection?: AzureConnection | null; allowCli?: boolean} = {},
): ConnectionValidation {
  const errors: ConnectionValidation['errors'] = {}
  const name = form.name.trim()
  if (!name) errors.name = 'required'
  else if ([...name].length > CONNECTION_NAME_MAX_LENGTH) errors.name = 'tooLong'

  let endpointWarning: AzureFieldErrorKey | null = null
  if (!form.endpoint.trim()) {
    errors.endpoint = 'required'
  } else {
    const key = endpointErrorKey(form.endpoint)
    if (key === 'providers.azureHostRejected') endpointWarning = key
    else if (key) errors.endpoint = key
  }

  if (form.authMode === 'entra_cli' && !opts.allowCli) errors.authMode = 'cliUnavailable'

  if (form.authMode === 'entra') {
    const tenant = form.tenantId.trim() ? tenantErrorKey(form.tenantId) : 'required'
    if (tenant) errors.tenantId = tenant
    const client = form.clientId.trim() ? guidErrorKey(form.clientId) : 'required'
    if (client) errors.clientId = client
  } else if (form.authMode === 'entra_cli') {
    const tenant = tenantErrorKey(form.tenantId)
    if (tenant) errors.tenantId = tenant
  }

  const carry = secretCarry(opts.connection, form)
  if ((carry === 'required' || carry === 'reenter') && !secretValue(form).trim()) errors.secret = 'required'

  return {errors, firstInvalid: CONNECTION_FIELD_ORDER.find(f => errors[f]) ?? null, endpointWarning}
}

export function toConnectionConfigInput(form: ConnectionFormState): AzureConnectionConfigInput {
  const auth: AzureFoundryAuthInput =
    form.authMode === 'api_key'
      ? {mode: 'api_key', ...(form.apiKey.trim() ? {key: form.apiKey.trim()} : {})}
      : form.authMode === 'entra_cli'
        ? {mode: 'entra_cli', ...(form.tenantId.trim() ? {tenantId: form.tenantId.trim()} : {})}
        : {
            mode: 'entra',
            tenantId: form.tenantId.trim(),
            clientId: form.clientId.trim(),
            ...(form.clientSecret.trim() ? {clientSecret: form.clientSecret.trim()} : {}),
          }
  const config: AzureConnectionConfigInput = {endpoint: form.endpoint.trim(), auth}
  if (form.projectEndpoint.trim()) config.projectEndpoint = form.projectEndpoint.trim()
  return config
}

export function toConnectionInput(form: ConnectionFormState): AzureConnectionCreateInput {
  return {name: form.name.trim(), config: toConnectionConfigInput(form)}
}

export function toConnectionUpdateInput(
  form: ConnectionFormState,
  baseline: ConnectionFormState,
  connection: AzureConnection,
): AzureConnectionUpdateInput {
  const out: AzureConnectionUpdateInput = {}
  const name = form.name.trim()
  if (name !== baseline.name.trim()) out.name = name
  const configChanged = CONFIG_FIELDS.some(k => form[k].trim() !== baseline[k].trim()) || !!secretValue(form).trim()
  if (configChanged) {
    out.config = toConnectionConfigInput(form)
    if (connection.credentialUpdatedAt) out.credentialUpdatedAt = connection.credentialUpdatedAt
  }
  if (connection.updatedAt) out.updatedAt = connection.updatedAt
  return out
}

export type ConnectionErrorTarget = ConnectionFormField | 'reindex' | 'conflict' | 'forbidden' | 'form'

const AUTH_DETAIL_FIELDS: Record<string, ConnectionFormField> = {
  mode: 'authMode',
  key: 'secret',
  tenantId: 'tenantId',
  clientId: 'clientId',
  clientSecret: 'secret',
}

export function connectionErrorTarget(err: unknown): ConnectionErrorTarget {
  const e = (err ?? {}) as {code?: unknown; status?: unknown; message?: unknown; details?: unknown}
  if (e.status === 403) return 'forbidden'
  const detail = e.details && typeof e.details === 'object' ? (e.details as Record<string, unknown>).field : undefined
  const detailField =
    typeof detail === 'string' && Object.prototype.hasOwnProperty.call(AUTH_DETAIL_FIELDS, detail)
      ? AUTH_DETAIL_FIELDS[detail]
      : undefined
  switch (e.code) {
    case 'INVALID_ENDPOINT':
    case 'HOST_NOT_ALLOWED':
      return 'endpoint'
    case 'INVALID_PROJECT_ENDPOINT':
      return 'projectEndpoint'
    case 'MISSING_SECRET':
      return detailField ?? 'secret'
    case 'INVALID_CONNECTION_NAME':
    case 'DUPLICATE_CONNECTION_NAME':
      return 'name'
    case 'INVALID_AUTH':
      return detailField ?? authErrorField(typeof e.message === 'string' ? e.message : '')
    case 'REINDEX_REQUIRED':
      return 'reindex'
    case 'CONFLICT':
      return 'conflict'
    default:
      return 'form'
  }
}

function authErrorField(message: string): ConnectionErrorTarget {
  const m = message.toLowerCase()
  if (m.includes('sign-in mode') || m.includes('authentication mode')) return 'authMode'
  if (m.includes('tenant')) return 'tenantId'
  if (m.includes('client id')) return 'clientId'
  if (m.includes('client secret') || m.includes('api key')) return 'secret'
  return 'form'
}
