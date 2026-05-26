import {invoke} from '@tauri-apps/api/core'
import {listen} from '@tauri-apps/api/event'

interface BackendConfig {
    port: number
    token: string
}

let config: BackendConfig | null = null

export async function getBackendConfig(): Promise<BackendConfig> {
    if (config) return config
    try {
        config = await invoke<BackendConfig>('get_backend_config')
        return config
    } catch (e) {
        // Wait for event if not ready
        return new Promise((resolve) => {
            listen<BackendConfig>('backend-ready', (event) => {
                config = event.payload
                resolve(config)
            })
        })
    }
}

export async function request<T>(path: string, body?: any, method: string = 'POST'): Promise<T> {
    const cfg = await getBackendConfig()
    const response = await fetch(`http://localhost:${cfg.port}${path}`, {
        method,
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${cfg.token}`
        },
        body: body ? JSON.stringify(body) : undefined
    })

    if (!response.ok) {
        const error = await response.json()
        throw new Error(error.error || 'Request failed')
    }

    return response.json()
}

// Event streaming support (SSE)
const listeners = new Set<(event: {name: string, data: any}) => void>()
let eventSource: EventSource | null = null

export async function subscribeToEvents(callback: (event: {name: string, data: any}) => void) {
    listeners.add(callback)
    
    if (!eventSource) {
        const cfg = await getBackendConfig()
        await new Promise<void>((resolve) => {
            const es = new EventSource(`http://localhost:${cfg.port}/api/events?token=${cfg.token}`)
            let settled = false
            const settle = () => {
                if (!settled) { settled = true; resolve() }
            }
            
            es.onopen = () => {
                console.log('SSE connected')
                settle()
            }
            es.onerror = (e) => {
                console.error('SSE error', e)
                es.close()
                eventSource = null
                settle()
            }
            es.onmessage = (event) => {
                try {
                    const data = JSON.parse(event.data)
                    console.log('SSE message', data)
                    listeners.forEach(l => l(data))
                } catch (err) {
                    console.error('SSE JSON error', err)
                }
            }
            eventSource = es
        })
    }

    return () => {
        listeners.delete(callback)
        if (listeners.size === 0 && eventSource) {
            eventSource.close()
            eventSource = null
        }
    }
}
