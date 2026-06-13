import { createAdapter } from '@/platform/adapters'
import {logger} from '@/lib/logger'

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

interface ResolvedConfig {
    apiUrl: string
    token: string
}

let configCache: ResolvedConfig | null = null
let configPromise: Promise<ResolvedConfig> | null = null

export async function getBackendConfig(): Promise<ResolvedConfig> {
    if (configCache) return configCache
    if (configPromise) return configPromise

    configPromise = createAdapter().getBackendConfig().then(cfg => {
        configCache = { apiUrl: cfg.apiUrl, token: cfg.token ?? '' }
        configPromise = null
        return configCache
    }).catch(err => {
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

let sessionToken: string | null = null;
export function setSessionToken(token: string | null): void {
    sessionToken = token;
}

// tokenExpired reports whether a JWT access token is at/near its `exp` (with a
// small clock-skew margin). Non-JWT tokens (the local-mode per-session token)
// and malformed tokens decode to "not expired" so local mode is unaffected.
function tokenExpired(token: string | null): boolean {
    if (!token) return false
    const parts = token.split('.')
    if (parts.length !== 3) return false // not a JWT (e.g. local-mode token)
    try {
        const payload = JSON.parse(atob(parts[1]))
        if (typeof payload.exp !== 'number') return false
        return payload.exp * 1000 <= Date.now() + 5_000 // refresh 5s early
    } catch {
        return false
    }
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
            refreshInFlight = refreshCallback().finally(() => { refreshInFlight = null })
        }
        await refreshInFlight
        invalidateConfigCache()
    } catch {
        // Refresh failed — let the request proceed and the 401 path handle it.
    }
}

async function doFetch(path: string, body: unknown, method: string): Promise<Response> {
    const cfg = await getBackendConfig()
    const token = sessionToken || cfg.token;
    return fetch(`${cfg.apiUrl}${path}`, {
        method,
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${token}`,
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
    })
}

// Auth endpoints must never trigger the 401→refresh retry: a 401 from the
// refresh endpoint would recursively call refresh again, producing a storm.
const AUTH_PATHS = ['/api/auth/refresh', '/api/auth/login', '/api/auth/register']

export async function request<T>(path: string, body?: unknown, method: string = 'POST'): Promise<T> {
    // Proactively refresh an already-expired access token so we don't make a
    // doomed first request (and log a 401) before the retry below.
    await ensureFreshToken(path)

    let response = await doFetch(path, body, method)

    // Attempt one token refresh on 401.
    // refreshInFlight deduplicates concurrent 401s: without it, two simultaneous
    // expired-token requests would each call refreshCallback() with the same
    // refresh token; the second call's token is already rotated → 401 → logout.
    if (response.status === 401 && refreshCallback && !AUTH_PATHS.includes(path)) {
        try {
            if (!refreshInFlight) {
                refreshInFlight = refreshCallback().finally(() => { refreshInFlight = null })
            }
            await refreshInFlight
            invalidateConfigCache()   // force re-read of new token
            response = await doFetch(path, body, method)
        } catch {
            // refresh failed — fall through to throw below
        }
    }

    if (!response.ok) {
        const error = await response.json().catch(() => ({ error: 'Request failed' }))
        const msg = (error as { error?: string }).error || 'Request failed'
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

/**
 * Fetch a short-lived, single-use WebSocket connect ticket. The browser cannot
 * set an Authorization header on a WebSocket handshake, so instead of putting
 * the long-lived access token in the ws:// URL (where it leaks into proxy/server
 * logs and history) we exchange it here — over an authenticated POST — for a
 * ticket that is only valid for seconds and only once.
 */
export async function getWsTicket(): Promise<string> {
    const res = await request<{ ticket: string; expiresAt: string }>('/api/ws-ticket')
    return res.ticket
}

// Event streaming support (SSE) via fetch (lets us send the token in a header
// instead of the URL). The connection auto-reconnects with exponential backoff
// so a transient network/server drop doesn't silently stop live updates.

export type EventConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting'

export type EventCallback = (event: { name: string; data: unknown }) => void

const listeners = new Set<EventCallback>()
const connectionListeners = new Set<(state: EventConnectionState) => void>()

let connectionState: EventConnectionState = 'idle'
let eventAbortController: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectAttempt = 0
let streamActive = false // true while at least one subscriber wants the stream

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

    try {
        const response = await fetch(`${cfg.apiUrl}/api/events`, {
            method: 'GET',
            headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream' },
            signal: controller.signal,
        })

        if (response.status === 401 && refreshCallback) {
            // Token expired mid-session: refresh once, then let the backoff retry.
            try {
                await refreshCallback()
                invalidateConfigCache()
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

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''
        while (true) {
            const { done, value } = await reader.read()
            if (done) break
            buffer += decoder.decode(value, { stream: true })
            const lines = buffer.split('\n')
            buffer = lines.pop() || ''
            for (const line of lines) {
                if (line.startsWith('data: ')) {
                    try {
                        const data = JSON.parse(line.slice(6))
                        listeners.forEach(l => l(data))
                    } catch (err) {
                        logger.warn('SSE JSON error', err)
                    }
                }
            }
        }
        // Stream ended (server closed the connection) — reconnect if still wanted.
        if (eventAbortController === controller) eventAbortController = null
        scheduleReconnect()
    } catch (err: unknown) {
        if (eventAbortController === controller) eventAbortController = null
        if (err instanceof DOMException && err.name === 'AbortError') return // intentional close, no reconnect
        scheduleReconnect()
    }
}

export async function subscribeToEvents(callback: EventCallback) {
    listeners.add(callback)

    if (!streamActive) {
        streamActive = true
        reconnectAttempt = 0
        void connectEvents()
    }

    return () => {
        listeners.delete(callback)
        if (listeners.size === 0) {
            // Last subscriber left — tear the connection down for good.
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
        }
    }
}
