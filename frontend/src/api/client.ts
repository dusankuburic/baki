import {createAdapter} from '@/platform/adapters'
import {logger} from '@/lib/logger'
import {decodeJwtPayload} from '@/lib/jwt'
// Type-only: keeps zod out of the eager module graph (schemas build lazily).
import type {z} from 'zod'

// ApiError carries the backend's error envelope: status, machine-readable
// `code` to branch on, and requestId for log correlation. Prefer code/status
// over the message (masked on 5xx, may be reworded).
export class ApiError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly code: string | null = null,
    public readonly requestId: string | null = null,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export class PermissionDeniedError extends ApiError {
  constructor(message: string, code: string | null = null, requestId: string | null = null) {
    super(message, 403, code, requestId)
    this.name = 'PermissionDeniedError'
  }
}

export class VersionConflictError extends ApiError {
  constructor(message: string, code: string | null = null, requestId: string | null = null) {
    super(message, 409, code, requestId)
    this.name = 'VersionConflictError'
  }
}

// Thrown when a validated response fails its zod schema — surfaces shape
// drift at the boundary instead of an opaque downstream TypeError.
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
      // Reset so the next caller can retry.
      configPromise = null
      throw err
    })

  return configPromise
}

/** Clear the cached backend config so the next call re-reads from storage. */
export function invalidateConfigCache(): void {
  configCache = null
  configPromise = null
}

// Optional callback invoked once on 401 before retrying a request.
// Registered by authStore to trigger token refresh.
let refreshCallback: (() => Promise<void>) | null = null

// Deduplicates concurrent refresh calls: one in-flight promise shared by all
// callers, so a rotated refresh token is never reused.
let refreshInFlight: Promise<void> | null = null

export function registerRefreshCallback(fn: () => Promise<void>): void {
  refreshCallback = fn
}

let sessionToken: string | null = null
export function setSessionToken(token: string | null): void {
  sessionToken = token
}

// tokenExpired reports whether a JWT access token is at/near its `exp` (with a
// small clock-skew margin). Non-JWT tokens (the local-mode per-session token)
// and malformed tokens decode to "not expired" so local mode is unaffected.
function tokenExpired(token: string | null): boolean {
  if (!token) return false
  const payload = decodeJwtPayload(token)
  if (!payload) return false // not a JWT (e.g. local-mode token) → not expired
  const exp = payload.exp
  if (typeof exp !== 'number') return false
  return exp * 1000 <= Date.now() + 5_000 // refresh 5s early
}

// Refreshes an already-expired access token before sending, sharing the
// refreshInFlight dedup with the 401 path.
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
    // Refresh failed — let the request proceed and the 401 path handle it.
  }
}

// Shared refresh-and-invalidate sequence for every 401 path. Callers keep
// their own guards and try/catch (post-refresh behaviour differs).
async function refreshOnUnauthorized(): Promise<void> {
  if (!refreshInFlight) {
    refreshInFlight = refreshCallback!().finally(() => {
      refreshInFlight = null
    })
  }
  await refreshInFlight
  invalidateConfigCache()
}

// Metadata calls only (AI responses stream separately over SSE); slow
// endpoints (bulk upload, batch analysis) override at their call site.
const DEFAULT_TIMEOUT_MS = 30_000
const DEFAULT_BLOB_TIMEOUT_MS = 90_000

// In-flight controllers, so logout can abort everything still running.
const activeRequests = new Set<AbortController>()

/**
 * Abort every in-flight request (called on logout / auth teardown). Aborted
 * requests reject with a DOMException AbortError; consumers that care can
 * check `err.name === 'AbortError'`.
 */
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
  // A pre-aborted signal short-circuits before touching the network.
  if (signal?.aborted) throw signal.reason ?? new DOMException('Aborted', 'AbortError')
  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const controller = new AbortController()
  activeRequests.add(controller)
  // `timedOut` is set before abort() so the catch can distinguish timeouts
  // (remapped Error) from consumer/abortAllRequests aborts (AbortError).
  let timedOut = false
  const timer = setTimeout(() => {
    timedOut = true
    controller.abort()
  }, timeoutMs)
  // External aborts share the timeout controller's abort path.
  const onExternalAbort = () => controller.abort(signal?.reason)
  signal?.addEventListener('abort', onExternalAbort, {once: true})
  try {
    return await fetch(`${cfg.apiUrl}${path}`, {
      method,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
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

// Auth endpoints must never trigger the 401→refresh retry: a 401 from the
// refresh endpoint would recursively call refresh again, producing a storm.
const AUTH_PATHS = ['/api/auth/refresh', '/api/auth/login', '/api/auth/register']

// On 401, one deduplicated token refresh + refetch. Shared by request() and
// requestBlob().
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
      // refresh failed — fall through to caller's non-ok handling
    }
  }
  return response
}

// Retryable: network-level failure (fetch TypeError) or 5xx. Never 4xx.
function isTransientFailure(err: unknown, status: number): boolean {
  if (status >= 500 && status < 600) return true
  return err instanceof TypeError
}

// Only safe-to-repeat methods retry (a retried mutation could double-apply).
const IDEMPOTENT_METHODS = new Set(['GET', 'HEAD'])

const MAX_RETRIES = 2
const RETRY_BASE_MS = 200

// Bounded retry (idempotent methods only) for transient failures.
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
      throw err
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, ms))
}

// Spreads retry attempts against a recovering server.
function jitter(attempt: number): number {
  const base = RETRY_BASE_MS * 2 ** attempt
  return base + Math.floor(Math.random() * base * 0.5)
}

export interface RequestOptions {
  /** JSON-serialized request body. Omit for bodyless requests (GET/DELETE). */
  body?: unknown
  /** HTTP method. Defaults to 'POST'. */
  method?: string
  /** Abort/timeout in ms. Defaults to DEFAULT_TIMEOUT_MS (30s). */
  timeoutMs?: number
  /** Opt OUT of GET dedup/cache (default false). Mutations never dedup. */
  noDedup?: boolean
  /**
   * Consumer-provided cancellation. Aborting rejects the request with a
   * DOMException AbortError (never remapped to "Request timed out", never
   * retried) — check `err.name === 'AbortError'`. Note: a deduped GET shares
   * one round trip, so aborting one caller does not abort the shared fetch
   * while other callers still await it.
   */
  signal?: AbortSignal
}

// --- GET dedup + micro-cache ---
// Identical in-flight GETs share one round trip; repeat GETs within the TTL
// are served from cache. Non-GETs bypass both; logout clears everything.

const GET_CACHE_TTL_MS = 5_000
const getInFlight = new Map<string, Promise<unknown>>()
const getCache = new Map<string, {value: unknown; expiresAt: number}>()

// Bumped by clearRequestCache(); a response resolving after a clear must not
// write the old session's data back into the cache.
let cacheEpoch = 0

/** Clears the GET micro-cache and in-flight dedup entries (logout/org switch). */
export function clearRequestCache(): void {
  cacheEpoch++
  getCache.clear()
  getInFlight.clear()
}

// Every consumer gets its own deep copy — cached values are parsed JSON, so
// in-place mutations by one consumer can't poison the cache or other readers.
function cloneCached<T>(value: T): T {
  if (typeof structuredClone === 'function') return structuredClone(value)
  return JSON.parse(JSON.stringify(value)) as T
}

// Drops expired entries on writes so the map can't grow unboundedly.
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
}

/**
 * Extract the structured error envelope from an error response body. The
 * backend emits the standard envelope `{code, message, requestId}`
 * (render.Error); a few legacy/proxy paths may still return a bare
 * `{error: string}` — accept both so the user sees the server's actual reason
 * instead of a generic "Request failed". The machine-readable `code` and
 * `requestId` ride along on ApiError so callers can branch on them.
 */
async function parseErrorEnvelope(response: Response): Promise<ErrorEnvelope> {
  const body = await response.json().catch(() => null)
  if (body && typeof body === 'object') {
    const b = body as {message?: unknown; error?: unknown; code?: unknown; requestId?: unknown}
    const message =
      typeof b.message === 'string' && b.message ? b.message : typeof b.error === 'string' && b.error ? b.error : ''
    if (message) {
      return {
        message,
        code: typeof b.code === 'string' && b.code ? b.code : null,
        requestId: typeof b.requestId === 'string' && b.requestId ? b.requestId : null,
      }
    }
  }
  return {message: 'Request failed', code: null, requestId: null}
}

// Raw transport: refresh + fetch + retry + envelope error mapping (no caching).
async function doRequest<T>(path: string, opts: RequestOptions): Promise<T> {
  const {body, method = 'POST', timeoutMs = DEFAULT_TIMEOUT_MS} = opts
  await ensureFreshToken(path)

  const response = await fetchWithRetry(path, body, method, timeoutMs, opts.signal)

  if (!response.ok) {
    const env = await parseErrorEnvelope(response)
    if (response.status === 403) {
      throw new PermissionDeniedError(env.message, env.code, env.requestId)
    }
    if (response.status === 409) {
      throw new VersionConflictError(env.message, env.code, env.requestId)
    }
    throw new ApiError(env.message, response.status, env.code, env.requestId)
  }

  // A 200 with a non-JSON body (misconfigured proxy) must throw a clean Error.
  return response.json().catch(() => {
    throw new Error('Server returned a non-JSON response')
  }) as Promise<T>
}

// Public entry: adds GET dedup + the 5s micro-cache on top of doRequest.
export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const {method = 'POST'} = opts
  const cacheable = !opts.noDedup && !opts.body && (method === 'GET' || method === 'get')
  if (!cacheable) {
    return doRequest<T>(path, opts)
  }

  const key = path
  const epoch = cacheEpoch
  const cached = getCache.get(key)
  if (cached && cached.expiresAt > Date.now()) {
    return cloneCached(cached.value) as T
  }
  const existing = getInFlight.get(key)
  if (existing) {
    // Share the round trip, not the object: clone from the pristine cached
    // copy; on an epoch mismatch fall back to the propagated value.
    return existing.then(v => {
      const entry = epoch === cacheEpoch ? getCache.get(key) : undefined
      return cloneCached(entry ? entry.value : v)
    }) as Promise<T>
  }

  const promise = doRequest<T>(path, opts)
    .then(value => {
      if (epoch !== cacheEpoch) return value
      evictExpiredCache()
      // Cache a pristine copy; the original goes only to the initiating caller.
      getCache.set(key, {value: cloneCached(value), expiresAt: Date.now() + GET_CACHE_TTL_MS})
      return value
    })
    .finally(() => {
      if (getInFlight.get(key) === promise) getInFlight.delete(key)
    })
  getInFlight.set(key, promise)
  return promise
}

// Fetches and validates against a zod schema — for high-risk endpoints where
// shape drift must fail loudly at the boundary.
export async function requestValidated<T>(path: string, schema: z.ZodType<T>, opts: RequestOptions = {}): Promise<T> {
  const raw = await request<unknown>(path, opts)
  const result = schema.safeParse(raw)
  if (!result.success) {
    throw new ResponseValidationError(path, result.error)
  }
  return result.data
}

// Binary/file downloads — same refresh/retry behaviour, Blob result.
export async function requestBlob(
  path: string,
  opts: {method?: string; timeoutMs?: number; signal?: AbortSignal} = {},
): Promise<Blob> {
  const {method = 'GET', timeoutMs = DEFAULT_BLOB_TIMEOUT_MS} = opts
  await ensureFreshToken(path)
  const response = await fetchWithRetry(path, undefined, method, timeoutMs, opts.signal)
  if (!response.ok) {
    const env = await parseErrorEnvelope(response)
    throw new ApiError(env.message, response.status, env.code, env.requestId)
  }
  return response.blob()
}

/**
 * Exchanges the access token (over an authenticated POST) for a short-lived,
 * single-use WebSocket connect ticket — the browser can't set Authorization
 * headers on a WS handshake, and tokens in URLs leak into logs.
 */
export async function getWsTicket(): Promise<string> {
  const res = await request<{ticket: string; expiresAt: string}>('/api/ws-ticket')
  return res.ticket
}

// SSE via fetch (token in a header, not the URL), with auto-reconnect.

export type EventConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting'

export type EventCallback = (event: {name: string; data: unknown}) => void

const listeners = new Set<EventCallback>()
const connectionListeners = new Set<(state: EventConnectionState) => void>()

// No bytes (not even the 20s heartbeat ping) for this long → half-dead socket;
// tear down and reconnect. ~2.25× the heartbeat interval tolerates one lost ping.
const SSE_INACTIVITY_TIMEOUT_MS = 45_000

let connectionState: EventConnectionState = 'idle'
let eventAbortController: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectAttempt = 0
let streamActive = false // true while at least one subscriber wants the stream
let teardownTimer: ReturnType<typeof setTimeout> | null = null // pending last-unsubscriber teardown

// Consecutive-failure budget: after this many attempts the loop stops
// ('idle') until the browser 'online' event or a new subscriber restarts it.
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

// Per-listener try/catch: a throwing consumer must not be misread by the
// read loop's outer catch as a connection failure.
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

/** Subscribe to SSE connection-state changes (for a live/reconnecting indicator). */
export function subscribeConnectionState(cb: (state: EventConnectionState) => void): () => void {
  connectionListeners.add(cb)
  cb(connectionState)
  return () => {
    connectionListeners.delete(cb)
  }
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
  // 1s, 2s, 4s, 8s … capped at 30s.
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

  // Refresh an expired token before connecting so we don't open with a doomed 401.
  await ensureFreshToken('/api/events')

  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const controller = new AbortController()
  eventAbortController = controller

  // Inactivity watchdog: on timeout, abort with `stalled` set so the catch
  // reconnects instead of treating it as an intentional teardown.
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

  // Armed before the dial so a black-holed connect also trips the watchdog.
  armInactivity()

  try {
    const response = await fetch(`${cfg.apiUrl}/api/events`, {
      method: 'GET',
      headers: {Authorization: `Bearer ${token}`, Accept: 'text/event-stream'},
      signal: controller.signal,
    })

    if (response.status === 401 && refreshCallback) {
      try {
        await refreshOnUnauthorized()
        reconnectAttempt = 0
      } catch {
        // refresh failed — fall through to reconnect/backoff
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
    while (true) {
      const {done, value} = await reader.read()
      if (done) break
      armInactivity()
      buffer += decoder.decode(value, {stream: true})
      const lines = buffer.split('\n')
      buffer = lines.pop() || ''
      for (const line of lines) {
        // "data:" with or without the single spec-allowed space after it.
        if (line.startsWith('data:')) {
          let payload = line.slice(5)
          if (payload.startsWith(' ')) payload = payload.slice(1)
          try {
            const data = JSON.parse(payload)
            dispatchSafely(listeners, l => l(data))
          } catch (err) {
            logger.warn('SSE JSON error', err)
          }
        }
      }
    }
    // Flush the decoder's buffer (partial multibyte sequence at stream end).
    buffer += decoder.decode()
    if (buffer.startsWith('data:')) {
      let payload = buffer.slice(5)
      if (payload.startsWith(' ')) payload = payload.slice(1)
      try {
        const data = JSON.parse(payload)
        dispatchSafely(listeners, l => l(data))
      } catch {
        // Trailing partial line at stream end — ignore.
      }
    }
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    scheduleReconnect()
  } catch (err: unknown) {
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    if (err instanceof DOMException && err.name === 'AbortError') {
      // Watchdog abort = dead connection → reconnect. Only teardown aborts stop.
      if (!stalled) return
      logger.warn(`SSE inactive for ${SSE_INACTIVITY_TIMEOUT_MS / 1000}s, reconnecting`)
    }
    scheduleReconnect()
  }
}

// Grace before teardown on the last unsubscribe — effect remounts
// (StrictMode, hot reload) resubscribe almost immediately.
const TEARDOWN_GRACE_MS = 5_000

export async function subscribeToEvents(callback: EventCallback) {
  listeners.add(callback)

  // A subscriber arriving during the grace period keeps the connection.
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
