import {createAdapter} from '@/platform/adapters'
import {logger} from '@/lib/logger'
import {decodeJwtPayload} from '@/lib/jwt'
import {z} from 'zod'

export class PermissionDeniedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'PermissionDeniedError'
  }
}

export class VersionConflictError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'VersionConflictError'
  }
}

// ResponseValidationError is thrown when a validated response fails its zod
// schema. This surfaces backend/proxy shape drift at the API boundary instead
// of letting it cascade into an opaque TypeError deep in a component.
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
      // Reset so the next caller can retry instead of receiving the same
      // rejected promise forever.
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

// Deduplicate concurrent refresh calls. If two requests both get 401 at the
// same time, both would call refreshCallback() with the same refresh token.
// The second call would use the now-rotated (invalid) token and fail, clearing
// auth state and logging the user out. Sharing one in-flight promise means all
// concurrent callers wait for the same refresh result.
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

// ensureFreshToken proactively refreshes an expired access token BEFORE sending
// a request, so a mid-session token expiry doesn't produce a doomed 401 (and a
// red console error) on every call before the transparent retry. Shares the
// same refreshInFlight dedup as the 401 path so concurrent callers refresh once.
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

// refreshOnUnauthorized runs the shared refresh-and-invalidate sequence used by
// every 401 path: dedupe concurrent refreshes via refreshInFlight (see comment
// above), then invalidate the cached backend config so the next request picks
// up the rotated token. Callers keep their own status/refreshCallback guard
// and try/catch around this, since what happens after a successful refresh
// differs (inline retry vs. reconnect-and-backoff).
async function refreshOnUnauthorized(): Promise<void> {
  if (!refreshInFlight) {
    refreshInFlight = refreshCallback!().finally(() => {
      refreshInFlight = null
    })
  }
  await refreshInFlight
  invalidateConfigCache()
}

// Default timeout for a normal request/requestBlob call. Everything under
// request()/requestBlob() is a metadata call against our own backend (the
// actual AI response streams separately over SSE via connectEvents), so this
// only needs to cover ordinary DB-backed round trips — a hung connection
// shouldn't be able to block the UI indefinitely. A handful of genuinely slow
// endpoints (bulk flow upload/folder-load, folder-wide batch analysis) pass an
// explicit longer override at their call site.
const DEFAULT_TIMEOUT_MS = 30_000
// requestBlob is used for file downloads (e.g. the account data-export bundle),
// which can legitimately take longer than a typical JSON round trip.
const DEFAULT_BLOB_TIMEOUT_MS = 90_000

async function doFetch(path: string, body: unknown, method: string, timeoutMs: number): Promise<Response> {
  const cfg = await getBackendConfig()
  const token = sessionToken || cfg.token
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
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
      throw new Error(`Request timed out: ${path}`, {cause: err})
    }
    throw err
  } finally {
    clearTimeout(timer)
  }
}

// Auth endpoints must never trigger the 401→refresh retry: a 401 from the
// refresh endpoint would recursively call refresh again, producing a storm.
const AUTH_PATHS = ['/api/auth/refresh', '/api/auth/login', '/api/auth/register']

// fetchWithRefresh performs a request and, on 401, attempts a single token
// refresh + refetch (deduplicated via refreshInFlight). Shared by request() and
// requestBlob() so the refresh logic isn't duplicated (it previously was, with
// divergence risk).
async function fetchWithRefresh(path: string, body: unknown, method: string, timeoutMs: number): Promise<Response> {
  let response = await doFetch(path, body, method, timeoutMs)
  if (response.status === 401 && refreshCallback && !AUTH_PATHS.includes(path)) {
    try {
      await refreshOnUnauthorized()
      response = await doFetch(path, body, method, timeoutMs)
    } catch {
      // refresh failed — fall through to caller's non-ok handling
    }
  }
  return response
}

// isTransientFailure reports whether a fetch error or status warrants a retry:
// a network-level failure (TypeError from fetch) or a 5xx server error. 4xx are
// not retried (the request itself is the problem, not a transient blip).
function isTransientFailure(err: unknown, status: number): boolean {
  if (status >= 500 && status < 600) return true
  // fetch() rejects with a TypeError on network failure / DNS / connection refused.
  return err instanceof TypeError
}

// isIdempotent limits retries to safe-to-repeat methods (GET/HEAD). POST/PUT/
// DELETE/PATCH are not retried automatically because a partial success could
// double-apply a side effect.
const IDEMPOTENT_METHODS = new Set(['GET', 'HEAD'])

const MAX_RETRIES = 2
const RETRY_BASE_MS = 200

// fetchWithRetry wraps fetchWithRefresh with a bounded retry for idempotent
// methods on transient failures (network blip, 502/503/504). Non-idempotent
// methods and 4xx pass through without retry. This keeps a momentary proxy hiccup
// during a settings save or findings-fetch from surfacing as a hard user error.
async function fetchWithRetry(path: string, body: unknown, method: string, timeoutMs: number): Promise<Response> {
  const canRetry = IDEMPOTENT_METHODS.has(method.toUpperCase())
  for (let attempt = 0; ; attempt++) {
    try {
      const response = await fetchWithRefresh(path, body, method, timeoutMs)
      if (canRetry && attempt < MAX_RETRIES && isTransientFailure(null, response.status)) {
        await sleep(jitter(attempt))
        continue
      }
      return response
    } catch (err) {
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

// jitter spreads retry attempts to avoid synchronised thundering-herd retries
// against a recovering server. ~200ms, ~400ms for attempts 0/1.
function jitter(attempt: number): number {
  const base = RETRY_BASE_MS * 2 ** attempt
  return base + Math.floor(Math.random() * base * 0.5)
}

// RequestOptions is the options bag for request()/requestValidated().
// Passing an explicit method/timeoutMs no longer requires positional
// `undefined` placeholders for the args you don't care about (the old
// request(path, body?, method?, timeoutMs?) signature forced every body-less
// GET to read `request('/x', undefined, 'GET')`).
export interface RequestOptions {
  /** JSON-serialized request body. Omit for bodyless requests (GET/DELETE). */
  body?: unknown
  /** HTTP method. Defaults to 'POST'. */
  method?: string
  /** Abort/timeout in ms. Defaults to DEFAULT_TIMEOUT_MS (30s). */
  timeoutMs?: number
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const {body, method = 'POST', timeoutMs = DEFAULT_TIMEOUT_MS} = opts
  // Proactively refresh an already-expired access token so we don't make a
  // doomed first request (and log a 401) before the retry below.
  await ensureFreshToken(path)

  const response = await fetchWithRetry(path, body, method, timeoutMs)

  if (!response.ok) {
    const error = await response.json().catch(() => ({error: 'Request failed'}))
    const msg = (error as {error?: string}).error || 'Request failed'
    if (response.status === 403) {
      throw new PermissionDeniedError(msg)
    }
    if (response.status === 409) {
      throw new VersionConflictError(msg)
    }
    throw new Error(msg)
  }

  // Wrap the success-path JSON parse in a .catch() so a misconfigured proxy
  // returning a 200 with an HTML body throws a clean Error rather than an
  // unhandled SyntaxError that can crash the calling React component.
  return response.json().catch(() => {
    throw new Error('Server returned a non-JSON response')
  }) as Promise<T>
}

// requestValidated fetches a response and validates it against a zod schema
// before returning. Use this for high-risk endpoints (auth, analysis report,
// settings) where a shape change should fail loudly at the boundary instead of
// producing an opaque downstream TypeError.
export async function requestValidated<T>(
  path: string,
  schema: z.ZodType<T>,
  opts: RequestOptions = {},
): Promise<T> {
  const raw = await request<unknown>(path, opts)
  const result = schema.safeParse(raw)
  if (!result.success) {
    throw new ResponseValidationError(path, result.error)
  }
  return result.data
}

// requestBlob is request() for binary/file downloads (e.g. the data-export
// bundle). It mirrors request()'s proactive-refresh + 401-retry behaviour but
// returns the response body as a Blob instead of parsing JSON. Uses the shared
// fetchWithRetry so transient 5xx/network failures on GET downloads also retry.
export async function requestBlob(
  path: string,
  opts: {method?: string; timeoutMs?: number} = {},
): Promise<Blob> {
  const {method = 'GET', timeoutMs = DEFAULT_BLOB_TIMEOUT_MS} = opts
  await ensureFreshToken(path)
  const response = await fetchWithRetry(path, undefined, method, timeoutMs)
  if (!response.ok) {
    const error = await response.json().catch(() => ({error: 'Request failed'}))
    throw new Error((error as {error?: string}).error || 'Request failed')
  }
  return response.blob()
}

/**
 * Fetch a short-lived, single-use WebSocket connect ticket. The browser cannot
 * set an Authorization header on a WebSocket handshake, so instead of putting
 * the long-lived access token in the ws:// URL (where it leaks into proxy/server
 * logs and history) we exchange it here — over an authenticated POST — for a
 * ticket that is only valid for seconds and only once.
 */
export async function getWsTicket(): Promise<string> {
  const res = await request<{ticket: string; expiresAt: string}>('/api/ws-ticket')
  return res.ticket
}

// Event streaming support (SSE) via fetch (lets us send the token in a header
// instead of the URL). The connection auto-reconnects with exponential backoff
// so a transient network/server drop doesn't silently stop live updates.

export type EventConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting'

export type EventCallback = (event: {name: string; data: unknown}) => void

const listeners = new Set<EventCallback>()
const connectionListeners = new Set<(state: EventConnectionState) => void>()

// The backend writes an SSE comment-frame heartbeat every 20s
// (sseHeartbeatInterval in internal/api/events.go). If NOTHING arrives for
// ~2.25× that — not even a ping — the socket is presumed half-dead (laptop
// suspend/resume, sidecar restart) and the connection is torn down so the
// backoff reconnect + delta-resume path can recover. Slack over 2× means one
// lost ping doesn't trigger a spurious reconnect.
const SSE_INACTIVITY_TIMEOUT_MS = 45_000

let connectionState: EventConnectionState = 'idle'
let eventAbortController: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectAttempt = 0
let streamActive = false // true while at least one subscriber wants the stream
let teardownTimer: ReturnType<typeof setTimeout> | null = null // pending last-unsubscriber teardown

function setConnectionState(s: EventConnectionState): void {
  if (connectionState === s) return
  connectionState = s
  connectionListeners.forEach(l => l(s))
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

  // Read-inactivity watchdog: any bytes (real events or the backend's
  // ": ping" heartbeat frames) count as liveness. On timeout, abort the
  // fetch with `stalled` set so the catch below reconnects instead of
  // treating it as an intentional teardown.
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

  // Armed BEFORE the dial: a fetch black-holed during the connect phase
  // (suspend/resume mid-dial, proxy accepting but never responding) must
  // trip the watchdog too, not just a stall after headers arrived.
  armInactivity()

  try {
    const response = await fetch(`${cfg.apiUrl}/api/events`, {
      method: 'GET',
      headers: {Authorization: `Bearer ${token}`, Accept: 'text/event-stream'},
      signal: controller.signal,
    })

    if (response.status === 401 && refreshCallback) {
      // Token expired mid-session: refresh once, then let the backoff retry.
      try {
        await refreshOnUnauthorized()
        // Refresh succeeded — the next attempt has a fresh token, so retry
        // promptly instead of inheriting the accumulated backoff delay.
        reconnectAttempt = 0
      } catch {
        // refresh failed — fall through to reconnect/backoff
      }
      throw new Error('SSE unauthorized')
    }
    if (!response.ok || !response.body) {
      throw new Error(`SSE HTTP ${response.status}`)
    }

    // Connected successfully — reset backoff.
    reconnectAttempt = 0
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
        // SSE spec: the field is "data:" optionally followed by a single space,
        // then the value. Handle both "data: {...}" (with space) and
        // "data:{...}" (no space) so a proxy that normalizes whitespace can't
        // corrupt the payload by slicing into the JSON.
        if (line.startsWith('data:')) {
          let payload = line.slice(5)
          if (payload.startsWith(' ')) payload = payload.slice(1)
          try {
            const data = JSON.parse(payload)
            listeners.forEach(l => l(data))
          } catch (err) {
            logger.warn('SSE JSON error', err)
          }
        }
      }
    }
    // Flush the decoder's internal buffer: a partial multibyte sequence held
    // across the final chunk boundary would otherwise be silently dropped.
    // (SSE payloads are typically ASCII-delimited JSON, so this rarely
    // matters, but it closes a correctness gap.)
    buffer += decoder.decode()
    if (buffer.startsWith('data:')) {
      let payload = buffer.slice(5)
      if (payload.startsWith(' ')) payload = payload.slice(1)
      try {
        const data = JSON.parse(payload)
        listeners.forEach(l => l(data))
      } catch {
        // A trailing partial line at stream end is expected (the server may
        // close mid-frame); ignore rather than warn.
      }
    }
    // Stream ended (server closed the connection) — reconnect if still wanted.
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    scheduleReconnect()
  } catch (err: unknown) {
    disarmInactivity()
    if (eventAbortController === controller) eventAbortController = null
    if (err instanceof DOMException && err.name === 'AbortError') {
      // A watchdog abort is a dead connection, not an intentional close —
      // fall through to reconnect. Only teardown aborts stop for good.
      if (!stalled) return
      logger.warn(`SSE inactive for ${SSE_INACTIVITY_TIMEOUT_MS / 1000}s, reconnecting`)
    }
    scheduleReconnect()
  }
}

// How long the connection survives with zero subscribers before being torn
// down. An effect remount (StrictMode double-invoke, hot reload, dep change)
// unsubscribes and resubscribes almost immediately; re-dialing the socket for
// that would re-open the connect window on every remount.
const TEARDOWN_GRACE_MS = 5_000

export async function subscribeToEvents(callback: EventCallback) {
  listeners.add(callback)

  // A subscriber arriving during the teardown grace period keeps the
  // existing connection instead of re-dialing.
  if (teardownTimer) {
    clearTimeout(teardownTimer)
    teardownTimer = null
  }

  if (!streamActive) {
    streamActive = true
    reconnectAttempt = 0
    void connectEvents()
  }

  return () => {
    listeners.delete(callback)
    if (listeners.size === 0 && !teardownTimer) {
      // Last subscriber left — tear the connection down after the grace
      // period, unless someone resubscribes first.
      teardownTimer = setTimeout(() => {
        teardownTimer = null
        if (listeners.size > 0) return
        streamActive = false
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
