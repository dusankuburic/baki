import {createAdapter} from '@/platform/adapters'
import {logger} from '@/lib/logger'
import {decodeJwtPayload} from '@/lib/jwt'
import {noteServerVersion, serverVersionOf} from '@/lib/buildVersion'
import type {z} from 'zod'

export class ApiError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly code: string | null = null,
    public readonly requestId: string | null = null,
    public readonly details?: Record<string, unknown>,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export class PermissionDeniedError extends ApiError {
  constructor(
    message: string,
    code: string | null = null,
    requestId: string | null = null,
    details?: Record<string, unknown>,
  ) {
    super(message, 403, code, requestId, details)
    this.name = 'PermissionDeniedError'
  }
}

export class VersionConflictError extends ApiError {
  constructor(
    message: string,
    code: string | null = null,
    requestId: string | null = null,
    details?: Record<string, unknown>,
  ) {
    super(message, 409, code, requestId, details)
    this.name = 'VersionConflictError'
  }
}

export class ResponseValidationError extends Error {
  constructor(
    public readonly path: string,
    public readonly cause: z.ZodError,
  ) {
    const issues = cause.issues.map(i => `${i.path.join('.') || '<root>'}: ${i.message}`).join('; ')
    super(`Response validation failed for ${path}: ${issues}`)
    this.name = 'ResponseValidationError'
  }
}

interface ResolvedConfig {
  apiUrl: string
  token: string
}

let configCache: ResolvedConfig | null = null
let configPromise: Promise<ResolvedConfig> | null = null

export async function getBackendConfig(): Promise<ResolvedConfig> {
  if (configCache) return configCache
  if (configPromise) return configPromise

  configPromise = createAdapter()
    .getBackendConfig()
    .then(cfg => {
      configCache = {apiUrl: cfg.apiUrl, token: cfg.token ?? ''}
      configPromise = null
      return configCache
    })
    .catch(err => {
      configPromise = null
      throw err
    })

  return configPromise
}

export function invalidateConfigCache(): void {
  configCache = null
  configPromise = null
}

let refreshCallback: (() => Promise<void>) | null = null

let refreshInFlight: Promise<void> | null = null

export function registerRefreshCallback(fn: () => Promise<void>): void {
  refreshCallback = fn
}

// The `id:` of the last frame the server sent. A reconnect sends it as Last-Event-ID so a server with the shared
// Redis replays what the gap missed. Empty until a server sends an id: an older server never does, so it never
// receives the header (a cross-origin preflight would reject it).
let lastEventId = ''
const SSE_MAX_EVENT_ID_LENGTH = 128

let sessionToken: string | null = null
export function setSessionToken(token: string | null): void {
  sessionToken = token
  // The last event id belongs to the signed-in user's log on the server: a sign-out forgets it.
  if (token === null) lastEventId = ''
}

function tokenExpired(token: string | null): boolean {
  if (!token) return false
  const payload = decodeJwtPayload(token)
  if (!payload) return false
  const exp = payload.exp
  if (typeof exp !== 'number') return false
  return exp * 1000 <= Date.now() + 5_000
}

async function ensureFreshToken(path: string): Promise<void> {
  if (!refreshCallback || AUTH_PATHS.includes(path)) return
  if (!tokenExpired(sessionToken)) return
  try {
    if (!refreshInFlight) {
      refreshInFlight = refreshCallback().finally(() => {
        refreshInFlight = null
      })
    }
    await refreshInFlight
    invalidateConfigCache()
  } catch {
  }
}

async function refreshOnUnauthorized(): Promise<void> {
  if (!refreshInFlight) {
    refreshInFlight = refreshCallback!().finally(() => {
      refreshInFlight = null
    })
  }
  await refreshInFlight
  invalidateConfigCache()
}

const DEFAULT_TIMEOUT_MS = 30_000
const DEFAULT_BLOB_TIMEOUT_MS = 90_000

const activeRequests = new Set<AbortController>()

export function abortAllRequests(): void {
  for (const controller of activeRequests) controller.abort()
  activeRequests.clear()
}

async function doFetch(
  path: string,
  body: unknown,
  method: string,
  timeoutMs: number,
  signal?: AbortSignal,
): Promise<Response> {
  if (signal?.aborted) throw signal.reason ?? new DOMException('Aborted', 'AbortError')
  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const controller = new AbortController()
  activeRequests.add(controller)
  let timedOut = false
  const timer = setTimeout(() => {
    timedOut = true
    controller.abort()
  }, timeoutMs)
  const onExternalAbort = () => controller.abort(signal?.reason)
  signal?.addEventListener('abort', onExternalAbort, {once: true})
  try {
    const response = await fetch(`${cfg.apiUrl}${path}`, {
      method,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
    noteServerVersion(serverVersionOf(response))
    return response
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') {
      if (!timedOut) throw err
      throw new Error(`Request timed out: ${path}`, {cause: err})
    }
    throw err
  } finally {
    clearTimeout(timer)
    signal?.removeEventListener('abort', onExternalAbort)
    activeRequests.delete(controller)
  }
}

const AUTH_PATHS = ['/api/auth/refresh', '/api/auth/login', '/api/auth/register']

async function fetchWithRefresh(
  path: string,
  body: unknown,
  method: string,
  timeoutMs: number,
  signal?: AbortSignal,
): Promise<Response> {
  let response = await doFetch(path, body, method, timeoutMs, signal)
  if (response.status === 401 && refreshCallback && !AUTH_PATHS.includes(path)) {
    try {
      await refreshOnUnauthorized()
      response = await doFetch(path, body, method, timeoutMs, signal)
    } catch {
    }
  }
  return response
}

function isTransientFailure(err: unknown, status: number): boolean {
  if (status >= 500 && status < 600) return true
  return err instanceof TypeError
}

const IDEMPOTENT_METHODS = new Set(['GET', 'HEAD'])

const MAX_RETRIES = 2
const RETRY_BASE_MS = 200

export const NETWORK_CODE = 'NETWORK'
const NETWORK_MESSAGE = "Couldn't reach the server — check your connection and try again."

function networkFailure(): ApiError {
  return new ApiError(NETWORK_MESSAGE, 0, NETWORK_CODE, null)
}

async function fetchWithRetry(
  path: string,
  body: unknown,
  method: string,
  timeoutMs: number,
  signal?: AbortSignal,
): Promise<Response> {
  const canRetry = IDEMPOTENT_METHODS.has(method.toUpperCase())
  for (let attempt = 0; ; attempt++) {
    try {
      const response = await fetchWithRefresh(path, body, method, timeoutMs, signal)
      if (canRetry && attempt < MAX_RETRIES && isTransientFailure(null, response.status)) {
        await sleep(jitter(attempt))
        continue
      }
      return response
    } catch (err) {
      if (signal?.aborted) throw err
      if (canRetry && attempt < MAX_RETRIES && isTransientFailure(err, 0)) {
        await sleep(jitter(attempt))
        continue
      }
      throw err instanceof TypeError ? networkFailure() : err
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, ms))
}

function jitter(attempt: number): number {
  const base = RETRY_BASE_MS * 2 ** attempt
  return base + Math.floor(Math.random() * base * 0.5)
}

export interface RequestOptions {
  body?: unknown
  method?: string
  timeoutMs?: number
  noDedup?: boolean
  signal?: AbortSignal
}

const GET_CACHE_TTL_MS = 5_000

export const GET_CACHE_MAX_BODY_CHARS = 512 * 1024

interface ParsedBody {
  value: unknown
  size: number
}

const getInFlight = new Map<string, Promise<ParsedBody>>()
const getCache = new Map<string, {value: unknown; expiresAt: number}>()

export function requestCacheSize(): number {
  return getCache.size
}

let cacheEpoch = 0

export function clearRequestCache(): void {
  cacheEpoch++
  getCache.clear()
  getInFlight.clear()
}

function cloneCached<T>(value: T): T {
  if (typeof structuredClone === 'function') return structuredClone(value)
  return JSON.parse(JSON.stringify(value)) as T
}

function evictExpiredCache(): void {
  const now = Date.now()
  for (const [key, entry] of getCache) {
    if (entry.expiresAt <= now) getCache.delete(key)
  }
}

interface ErrorEnvelope {
  message: string
  code: string | null
  requestId: string | null
  details?: Record<string, unknown>
}

async function parseErrorEnvelope(response: Response): Promise<ErrorEnvelope> {
  const body = await response.json().catch(() => null)
  if (body && typeof body === 'object') {
    const b = body as {message?: unknown; error?: unknown; code?: unknown; requestId?: unknown; details?: unknown}
    const message =
      typeof b.message === 'string' && b.message ? b.message : typeof b.error === 'string' && b.error ? b.error : ''
    if (message) {
      return {
        message,
        code: typeof b.code === 'string' && b.code ? b.code : null,
        requestId: typeof b.requestId === 'string' && b.requestId ? b.requestId : null,
        details:
          b.details && typeof b.details === 'object' && !Array.isArray(b.details)
            ? (b.details as Record<string, unknown>)
            : undefined,
      }
    }
  }
  return {message: 'Request failed', code: null, requestId: null}
}

const READ_METHODS = new Set(['GET', 'HEAD', 'OPTIONS'])

export const OFFLINE_CODE = 'OFFLINE'
const OFFLINE_MESSAGE = "You're offline — this change was not saved. Reconnect and try again."

function offlineMutation(method: string): boolean {
  return !READ_METHODS.has(method.toUpperCase()) && typeof navigator !== 'undefined' && navigator.onLine === false
}

async function errorFor(response: Response): Promise<ApiError> {
  const env = await parseErrorEnvelope(response)
  if (response.status === 403) return new PermissionDeniedError(env.message, env.code, env.requestId, env.details)
  if (response.status === 409) return new VersionConflictError(env.message, env.code, env.requestId, env.details)
  return new ApiError(env.message, response.status, env.code, env.requestId, env.details)
}

async function doRequest(path: string, opts: RequestOptions): Promise<ParsedBody> {
  const {body, method = 'POST', timeoutMs = DEFAULT_TIMEOUT_MS} = opts
  if (offlineMutation(method)) {
    throw new ApiError(OFFLINE_MESSAGE, 0, OFFLINE_CODE)
  }
  await ensureFreshToken(path)

  const response = await fetchWithRetry(path, body, method, timeoutMs, opts.signal)

  if (!response.ok) throw await errorFor(response)

  if (response.status === 204) return {value: undefined, size: 0}

  try {
    const text = await response.text()
    return {value: JSON.parse(text), size: text.length}
  } catch {
    throw new Error('Server returned a non-JSON response')
  }
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const {method = 'POST'} = opts
  const cacheable = !opts.noDedup && !opts.body && (method === 'GET' || method === 'get')
  if (!cacheable) {
    return (await doRequest(path, opts)).value as T
  }

  const key = path
  const epoch = cacheEpoch
  const cached = getCache.get(key)
  if (cached) {
    if (cached.expiresAt > Date.now()) return cloneCached(cached.value) as T
    getCache.delete(key)
  }
  const shareable = !opts.signal
  const existing = shareable ? getInFlight.get(key) : undefined
  if (existing) {
    return existing.then(res => {
      const entry = epoch === cacheEpoch ? getCache.get(key) : undefined
      if (entry) return cloneCached(entry.value)
      return res.size > GET_CACHE_MAX_BODY_CHARS ? res.value : cloneCached(res.value)
    }) as Promise<T>
  }

  const promise = doRequest(path, opts)
    .then(res => {
      if (epoch !== cacheEpoch || res.size > GET_CACHE_MAX_BODY_CHARS) return res
      evictExpiredCache()
      getCache.set(key, {value: cloneCached(res.value), expiresAt: Date.now() + GET_CACHE_TTL_MS})
      return res
    })
    .finally(() => {
      if (getInFlight.get(key) === promise) getInFlight.delete(key)
    })
  if (shareable) getInFlight.set(key, promise)
  return (await promise).value as T
}

export async function requestValidated<T>(path: string, schema: z.ZodType<T>, opts: RequestOptions = {}): Promise<T> {
  const raw = await request<unknown>(path, opts)
  const result = schema.safeParse(raw)
  if (!result.success) {
    throw new ResponseValidationError(path, result.error)
  }
  return result.data
}

export interface RawRequestOptions {
  body?: unknown
  method?: string
  timeoutMs?: number
  signal?: AbortSignal
}

async function requestRaw(path: string, opts: RawRequestOptions): Promise<Response> {
  const {body, method = body === undefined ? 'GET' : 'POST', timeoutMs = DEFAULT_BLOB_TIMEOUT_MS} = opts
  await ensureFreshToken(path)
  const response = await fetchWithRetry(path, body, method, timeoutMs, opts.signal)
  if (!response.ok) throw await errorFor(response)
  return response
}

export async function requestBlob(path: string, opts: RawRequestOptions = {}): Promise<Blob> {
  return (await requestRaw(path, opts)).blob()
}

// requestForm sends one file as a multipart/form-data "file" part. The fetch layer must not touch the body or
// set Content-Type: the browser draws the multipart boundary, and every other helper JSON-stringifies.
export async function requestForm<T>(path: string, file: File | Blob, field = 'avatar'): Promise<T> {
  await ensureFreshToken(path)
  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const body = new FormData()
  body.append(field, file, file instanceof File ? file.name : 'avatar')
  const response = await fetch(`${cfg.apiUrl}${path}`, {
    method: 'POST',
    headers: {Authorization: `Bearer ${token}`},
    body,
  })
  noteServerVersion(serverVersionOf(response))
  if (!response.ok) throw await errorFor(response)
  const text = await response.text()
  try {
    return JSON.parse(text) as T
  } catch {
    throw new Error('Server returned a non-JSON response')
  }
}

export async function requestText(path: string, opts: RawRequestOptions = {}): Promise<string> {
  return (await requestRaw(path, opts)).text()
}

export async function getWsTicket(): Promise<string> {
  const res = await request<{ticket: string; expiresAt: string}>('/api/ws-ticket')
  return res.ticket
}

export type EventConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting'

export type EventCallback = (event: {name: string; data: unknown}) => void

const listeners = new Set<EventCallback>()
const gapListeners = new Set<() => void>()
const connectionListeners = new Set<(state: EventConnectionState) => void>()

const SSE_INACTIVITY_TIMEOUT_MS = 45_000

let connectionState: EventConnectionState = 'idle'
let eventAbortController: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectAttempt = 0
let streamActive = false
let teardownTimer: ReturnType<typeof setTimeout> | null = null

// parseEventId is the value of an `id:` line, or null when it cannot be an id (empty, too long, or holding NUL,
// which the SSE specification says to ignore).
function parseEventId(line: string): string | null {
  let value = line.slice(3)
  if (value.startsWith(' ')) value = value.slice(1)
  if (value.endsWith('\r')) value = value.slice(0, -1)
  if (value === '' || value.length > SSE_MAX_EVENT_ID_LENGTH || value.includes('\0')) return null
  return value
}

const SSE_MAX_RECONNECT_ATTEMPTS = 10
let sseGivenUp = false
let onlineRetryHandler: (() => void) | null = null

function clearOnlineRetry(): void {
  if (onlineRetryHandler) {
    window.removeEventListener('online', onlineRetryHandler)
    onlineRetryHandler = null
  }
}

function setConnectionState(s: EventConnectionState): void {
  if (connectionState === s) return
  connectionState = s
  dispatchSafely(connectionListeners, l => l(s))
}

function dispatchSafely<T>(set: Set<T>, invoke: (listener: T) => void): void {
  for (const listener of set) {
    try {
      invoke(listener)
    } catch (err) {
      logger.error('SSE listener threw (isolated; connection unaffected)', err)
    }
  }
}

export function getEventConnectionState(): EventConnectionState {
  return connectionState
}

export function subscribeConnectionState(cb: (state: EventConnectionState) => void): () => void {
  connectionListeners.add(cb)
  cb(connectionState)
  return () => {
    connectionListeners.delete(cb)
  }
}

// The server could not replay everything a reconnect missed (more frames than its log carries, a cursor older than the
// log, or Redis unreadable) and says so with a replay:gap event, which is not handed to the event listeners: a gap
// subscriber refetches what it shows, as it does after a reconnect. Returns the unsubscribe function.
export function subscribeEventGap(cb: () => void): () => void {
  gapListeners.add(cb)
  return () => {
    gapListeners.delete(cb)
  }
}

const GAP_EVENT = 'replay:gap'

function deliverEvent(data: unknown): void {
  if (typeof data === 'object' && data !== null && (data as {name?: unknown}).name === GAP_EVENT) {
    dispatchSafely(gapListeners, l => l())
    return
  }
  dispatchSafely(listeners, l => l(data as Parameters<EventCallback>[0]))
}

function scheduleReconnect(): void {
  if (!streamActive || reconnectTimer) return
  if (reconnectAttempt >= SSE_MAX_RECONNECT_ATTEMPTS) {
    if (!sseGivenUp) {
      sseGivenUp = true
      setConnectionState('idle')
      logger.warn(
        `SSE reconnect budget exhausted (${SSE_MAX_RECONNECT_ATTEMPTS} attempts); waiting for network recovery`,
      )
      onlineRetryHandler = () => {
        onlineRetryHandler = null
        if (sseGivenUp && streamActive) {
          sseGivenUp = false
          reconnectAttempt = 0
          void connectEvents()
        }
      }
      window.addEventListener('online', onlineRetryHandler, {once: true})
    }
    return
  }
  const delay = Math.min(30_000, 1_000 * 2 ** reconnectAttempt)
  reconnectAttempt++
  setConnectionState('reconnecting')
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    void connectEvents()
  }, delay)
}

async function connectEvents(): Promise<void> {
  if (!streamActive) return
  setConnectionState(reconnectAttempt === 0 ? 'connecting' : 'reconnecting')

  await ensureFreshToken('/api/events')

  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const controller = new AbortController()
  eventAbortController = controller

  let stalled = false
  let inactivityTimer: ReturnType<typeof setTimeout> | null = null
  const armInactivity = () => {
    if (inactivityTimer) clearTimeout(inactivityTimer)
    inactivityTimer = setTimeout(() => {
      stalled = true
      controller.abort()
    }, SSE_INACTIVITY_TIMEOUT_MS)
  }
  const disarmInactivity = () => {
    if (inactivityTimer) {
      clearTimeout(inactivityTimer)
      inactivityTimer = null
    }
  }

  armInactivity()

  try {
    const response = await fetch(`${cfg.apiUrl}/api/events`, {
      method: 'GET',
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: 'text/event-stream',
        ...(lastEventId ? {'Last-Event-ID': lastEventId} : {}),
      },
      signal: controller.signal,
    })
    noteServerVersion(serverVersionOf(response))

    if (response.status === 401 && refreshCallback) {
      try {
        await refreshOnUnauthorized()
        reconnectAttempt = 0
      } catch {
      }
      throw new Error('SSE unauthorized')
    }
    if (!response.ok || !response.body) {
      throw new Error(`SSE HTTP ${response.status}`)
    }

    reconnectAttempt = 0
    sseGivenUp = false
    clearOnlineRetry()
    setConnectionState('open')
    armInactivity()

    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    // An `id:` line applies to the event whose `data:` line follows it, as in the SSE specification.
    let pendingId: string | null = null
    const takeId = () => {
      if (pendingId !== null) lastEventId = pendingId
      pendingId = null
    }
    while (true) {
      const {done, value} = await reader.read()
      if (done) break
      armInactivity()
      buffer += decoder.decode(value, {stream: true})
      const lines = buffer.split('\n')
      buffer = lines.pop() || ''
      for (const line of lines) {
        if (line.startsWith('id:')) {
          pendingId = parseEventId(line)
        } else if (line.startsWith('data:')) {
          let payload = line.slice(5)
          if (payload.startsWith(' ')) payload = payload.slice(1)
          takeId()
          try {
            deliverEvent(JSON.parse(payload))
          } catch (err) {
            logger.warn('SSE JSON error', err)
          }
        }
      }
    }
    buffer += decoder.decode()
    if (buffer.startsWith('id:')) {
      pendingId = parseEventId(buffer)
    } else if (buffer.startsWith('data:')) {
      let payload = buffer.slice(5)
      if (payload.startsWith(' ')) payload = payload.slice(1)
      takeId()
      try {
        deliverEvent(JSON.parse(payload))
      } catch {
      }
    }
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    scheduleReconnect()
  } catch (err: unknown) {
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    if (err instanceof DOMException && err.name === 'AbortError') {
      if (!stalled) return
      logger.warn(`SSE inactive for ${SSE_INACTIVITY_TIMEOUT_MS / 1000}s, reconnecting`)
    }
    scheduleReconnect()
  }
}

const TEARDOWN_GRACE_MS = 5_000

export async function subscribeToEvents(callback: EventCallback) {
  listeners.add(callback)

  if (teardownTimer) {
    clearTimeout(teardownTimer)
    teardownTimer = null
  }

  if (!streamActive) {
    streamActive = true
    reconnectAttempt = 0
    if (sseGivenUp) {
      sseGivenUp = false
      clearOnlineRetry()
    }
    void connectEvents()
  }

  return () => {
    listeners.delete(callback)
    if (listeners.size === 0 && !teardownTimer) {
      teardownTimer = setTimeout(() => {
        teardownTimer = null
        if (listeners.size > 0) return
        streamActive = false
        lastEventId = ''
        sseGivenUp = false
        clearOnlineRetry()
        if (reconnectTimer) {
          clearTimeout(reconnectTimer)
          reconnectTimer = null
        }
        if (eventAbortController) {
          eventAbortController.abort()
          eventAbortController = null
        }
        setConnectionState('idle')
      }, TEARDOWN_GRACE_MS)
    }
  }
}
