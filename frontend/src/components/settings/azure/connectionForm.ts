import type {
  AzureConnection,
  AzureConnectionConfigInput,
  AzureConnectionCreateInput,
  AzureConnectionPublicConfig,
  AzureConnectionUpdateInput,
  AzureFoundryAuthInput,
  AzureFoundryAuthMode,
} from '@/types'

// The Resource modal's pure half: the field validators, the secret-carry
// rule, the dirty check, the request bodies, and where a server error code
// belongs.

// Host suffixes the server's validator accepts for a resource endpoint
// (azureFoundryHostSuffixes in internal/ai/azure_foundry_config.go). The
// client check only catches obvious mistakes early — the server stays
// authoritative, and its HOST_NOT_ALLOWED error still names the accepted
// hosts for anything this list might miss.
const AZURE_HOST_SUFFIXES = ['.openai.azure.com', '.services.ai.azure.com', '.cognitiveservices.azure.com']

// Same GUID shape the server requires for the Entra client ID (and accepts
// for the tenant ID).
const GUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// A tenant may also be given by one of its domain names
// (contoso.onmicrosoft.com): at least two DNS labels, the server's
// azureTenantDomain rule. The multi-tenant pseudo-tenants are single labels,
// so they never match.
const TENANT_DOMAIN_RE = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/i

// The i18n keys (settings namespace) the field validators below can return
// (typed as literals so they flow straight into t()).
export type AzureFieldErrorKey =
  | 'providers.azureEndpointInvalid'
  | 'providers.azureEndpointHttps'
  | 'providers.azureHostRejected'
  | 'providers.azureGuidInvalid'
  | 'providers.azureTenantInvalid'

// Lightweight client-side mirrors of the server's endpoint rules: an absolute
// https:// URL whose host is an Azure AI Foundry one. Returns the i18n key of
// the problem (shown once the field has been blurred), or null.
// 'providers.azureHostRejected' is advisory only: operators can extend the
// server's host allow-list (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
export function endpointErrorKey(value: string): AzureFieldErrorKey | null {
  const raw = value.trim()
  if (!raw) return null
  let url: URL | null = null
  try {
    url = new URL(raw)
  } catch {
    /* unparseable — handled below */
  }
  if (!url || !url.hostname) return 'providers.azureEndpointInvalid'
  if (url.protocol !== 'https:') return 'providers.azureEndpointHttps'
  const host = url.hostname.toLowerCase()
  if (!AZURE_HOST_SUFFIXES.some(s => host.endsWith(s) && host.length > s.length)) return 'providers.azureHostRejected'
  return null
}

// The lower-cased host[:port] of an Azure endpoint URL, '' when it does
// not parse — the client mirror of the server's azureURLHost comparison.
export function azureHost(raw: string): string {
  try {
    return new URL(raw.trim()).host.toLowerCase()
  } catch {
    return ''
  }
}

// secretHostsKept mirrors the server's carry rule (azureSecretHostsKept):
// the endpoint host unchanged, and a project endpoint only on the previous
// project's or the endpoint's host. A blank or unparseable endpoint never
// carries. Used so the placeholder never promises a secret the save would
// refuse to carry.
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

// Entra tenant/client IDs are GUIDs on the server; blank is left to the
// existing required-field gating.
export function guidErrorKey(value: string): AzureFieldErrorKey | null {
  return value.trim() && !GUID_RE.test(value.trim()) ? 'providers.azureGuidInvalid' : null
}

// The tenant ID is a GUID or a domain name of the tenant (server rule).
export function tenantErrorKey(value: string): AzureFieldErrorKey | null {
  const v = value.trim()
  if (!v || GUID_RE.test(v)) return null
  const isIPv4 = /^\d{1,3}(\.\d{1,3}){3}$/.test(v)
  return v.length <= 253 && TENANT_DOMAIN_RE.test(v) && !isIPv4 ? null : 'providers.azureTenantInvalid'
}

// ---- the Resource modal's form ----

/** A resource name is 1–64 characters. */
export const CONNECTION_NAME_MAX_LENGTH = 64

export interface ConnectionFormState {
  name: string
  /** The user typed a name: it no longer follows the endpoint's host. */
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

// The fields that hold values (nameEdited is bookkeeping).
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

// The fields a credential change is made of (all but the name).
const CONFIG_FIELDS = ['endpoint', 'authMode', 'tenantId', 'clientId', 'projectEndpoint'] as const

/**
 * The sign-in mode a saved resource's form starts on. The server sends ''
 * (and blank settings) when the stored credential document is missing or
 * unreadable, and a newer server might name a mode this client doesn't know:
 * either way the form starts on a real one — Microsoft Entra ID when an Entra
 * principal is known, else an API key — so one sign-in mode is selected and
 * the credential fields shown are the ones validated and sent.
 */
export function formAuthMode(config: AzureConnectionPublicConfig): AzureFoundryAuthMode {
  const mode = config.authMode
  if (mode === 'api_key' || mode === 'entra' || mode === 'entra_cli') return mode
  return config.tenantId?.trim() || config.clientId?.trim() ? 'entra' : 'api_key'
}

/** The edit form of a saved resource: its public settings, secrets blank, the name its own. */
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

/** The name a new resource takes from its endpoint: the host's first label ("my-res"), '' when it doesn't parse. */
export function nameFromEndpoint(endpoint: string): string {
  let host: string
  try {
    host = new URL(endpoint.trim()).hostname
  } catch {
    return ''
  }
  return [...host.split('.')[0]].slice(0, CONNECTION_NAME_MAX_LENGTH).join('')
}

/** Sets the endpoint; the name follows its host until the user edits the name. */
export function withEndpoint(form: ConnectionFormState, endpoint: string): ConnectionFormState {
  return form.nameEdited ? {...form, endpoint} : {...form, endpoint, name: nameFromEndpoint(endpoint)}
}

/** Sets the name as typed; it stops following the endpoint. */
export function withName(form: ConnectionFormState, name: string): ConnectionFormState {
  return {...form, name, nameEdited: true}
}

/** True when any value differs from the baseline — the dirty check before discarding. */
export function formsDiffer(a: ConnectionFormState, b: ConnectionFormState): boolean {
  return VALUE_FIELDS.some(k => a[k] !== b[k])
}

/**
 * The secret field a sign-in mode shows: the API key, none for the Azure CLI
 * (its signed-in user is the credential), and otherwise — Microsoft Entra ID
 * — the app's client secret. The modal renders its credential fields by it,
 * so the secret validated, focused and sent is always the one on screen.
 */
export function secretField(mode: AzureFoundryAuthMode): 'apiKey' | 'clientSecret' | null {
  return mode === 'api_key' ? 'apiKey' : mode === 'entra_cli' ? null : 'clientSecret'
}

/** The secret the form's auth mode takes ('' for the Azure CLI, which has none). */
export function secretValue(form: ConnectionFormState): string {
  const field = secretField(form.authMode)
  return field ? form[field] : ''
}

/**
 * What a blank secret field means on save:
 *  - `kept` — the server carries the stored secret (same auth mode, same
 *    Entra principal, secretHostsKept): show "Leave blank to keep the
 *    stored secret".
 *  - `reenter` — a secret is stored but this edit drops the carry (the
 *    endpoint, project host or principal changed): show why it must be
 *    entered again.
 *  - `required` — nothing to carry (a new resource, no stored secret,
 *    another auth mode, or a resource whose credentials must be entered
 *    again — the "Enter credentials" re-auth).
 *  - `none` — the auth mode takes no secret (the Azure CLI).
 */
export type SecretCarry = 'kept' | 'reenter' | 'required' | 'none'

export function secretCarry(conn: AzureConnection | null | undefined, form: ConnectionFormState): SecretCarry {
  if (!secretField(form.authMode)) return 'none'
  if (!conn || !conn.config.hasSecret || conn.config.authMode !== form.authMode) return 'required'
  // A stored secret that is unreadable or disabled carries nothing usable.
  if (conn.status === 'needs_credentials' || conn.status === 'unreadable') return 'required'
  // Tenant and client compare trimmed and lower-cased, like the server's Normalize.
  const norm = (s?: string) => (s ?? '').trim().toLowerCase()
  const samePrincipal =
    form.authMode !== 'entra' ||
    (norm(form.tenantId) === norm(conn.config.tenantId) && norm(form.clientId) === norm(conn.config.clientId))
  return samePrincipal && secretHostsKept(conn.config, form) ? 'kept' : 'reenter'
}

/** The modal's fields in tab order (the first invalid one takes focus). */
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

/**
 * A field's problem: 'required', 'tooLong', 'cliUnavailable' (Azure CLI
 * sign-in outside the desktop app, or on an organisation's resource), or a
 * settings-namespace i18n key from the validators above.
 */
export type ConnectionFieldError = 'required' | 'tooLong' | 'cliUnavailable' | AzureFieldErrorKey

export interface ConnectionValidation {
  errors: Partial<Record<ConnectionFormField, ConnectionFieldError>>
  /** Where focus goes on submit; null when the form may be sent. */
  firstInvalid: ConnectionFormField | null
  /** The advisory host warning ('providers.azureHostRejected'); never blocks a save. */
  endpointWarning: AzureFieldErrorKey | null
}

/**
 * Validates on submit (the modal keeps Save enabled). `connection` is the
 * resource being edited (null for a new one); `allowCli` is true only for a
 * personal resource on a local backend.
 */
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

/** The resource settings as sent: trimmed, blank optional fields dropped, a blank secret left out (never ""). */
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

/** POST /connections. */
export function toConnectionInput(form: ConnectionFormState): AzureConnectionCreateInput {
  return {name: form.name.trim(), config: toConnectionConfigInput(form)}
}

/**
 * PUT /connections/{id} for an edit of `connection` whose form started as
 * `baseline`: the name when it changed, the settings when any of them changed
 * or a secret was typed (with the credential's compare-and-swap stamp), and
 * the resource's own stamp.
 */
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

/**
 * Where a failed save or test goes: a field, the re-index confirm
 * (`reindex`), the compare-and-swap alert with Reload (`conflict`), the
 * read-only copy (`forbidden`, a 403), or the form-level alert (`form`).
 * INVALID_AUTH and MISSING_SECRET name the part of the credential at fault
 * in `details.field` (mode | key | tenantId | clientId | clientSecret);
 * an INVALID_AUTH from a server that does not says it in its message.
 */
export type ConnectionErrorTarget = ConnectionFormField | 'reindex' | 'conflict' | 'forbidden' | 'form'

// The form field a credential's `details.field` names.
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

// INVALID_AUTH names the part of the credential at fault in its message
// (internal/ai/azure_foundry_config.go); anything unrecognised stays a
// form-level alert, where the message still reads right.
function authErrorField(message: string): ConnectionErrorTarget {
  const m = message.toLowerCase()
  if (m.includes('sign-in mode') || m.includes('authentication mode')) return 'authMode'
  if (m.includes('tenant')) return 'tenantId'
  if (m.includes('client id')) return 'clientId'
  if (m.includes('client secret') || m.includes('api key')) return 'secret'
  return 'form'
}
