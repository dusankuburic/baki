import { createAdapter } from '@/platform/adapters'

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

export function registerRefreshCallback(fn: () => Promise<void>): void {
    refreshCallback = fn
}

let sessionToken: string | null = null;
export function setSessionToken(token: string | null): void {
    sessionToken = token;
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

export async function request<T>(path: string, body?: unknown, method: string = 'POST'): Promise<T> {
    let response = await doFetch(path, body, method)

    // Attempt one token refresh on 401
    if (response.status === 401 && refreshCallback) {
        try {
            await refreshCallback()   // updates localStorage
            invalidateConfigCache()   // force re-read of new token
            response = await doFetch(path, body, method)
        } catch {
            // refresh failed — fall through to throw below
        }
    }

    if (!response.ok) {
        const error = await response.json().catch(() => ({ error: 'Request failed' }))
        throw new Error((error as { error?: string }).error || 'Request failed')
    }

    return response.json() as Promise<T>
}

// Event streaming support (SSE) via fetch to avoid token leakage in URL
const listeners = new Set<(event: {name: string, data: any}) => void>()
let eventAbortController: AbortController | null = null

export async function subscribeToEvents(callback: (event: {name: string, data: any}) => void) {
    listeners.add(callback)

    if (!eventAbortController) {
        const cfg = await getBackendConfig()
        const token = sessionToken || cfg.token;
        const controller = new AbortController()
        eventAbortController = controller

        fetch(`${cfg.apiUrl}/api/events`, {
            method: 'GET',
            headers: {
                'Authorization': `Bearer ${token}`,
                'Accept': 'text/event-stream'
            },
            signal: controller.signal
        }).then(async response => {
            const reader = response.body?.getReader();
            if (!reader) return;
            const decoder = new TextDecoder();
            let buffer = '';
            while (true) {
                const { done, value } = await reader.read();
                if (done) break;
                buffer += decoder.decode(value, { stream: true });
                const lines = buffer.split('\n');
                buffer = lines.pop() || '';
                for (const line of lines) {
                    if (line.startsWith('data: ')) {
                        try {
                            const data = JSON.parse(line.slice(6));
                            listeners.forEach(l => l(data));
                        } catch (err) {
                            console.error('SSE JSON error', err)
                        }
                    }
                }
            }
        }).catch(err => {
            if (err.name !== 'AbortError') {
                console.error('SSE fetch error', err);
            }
        }).finally(() => {
            if (eventAbortController === controller) {
                eventAbortController = null;
            }
        })
    }

    return () => {
        listeners.delete(callback)
        if (listeners.size === 0 && eventAbortController) {
            eventAbortController.abort()
            eventAbortController = null
        }
    }
}
