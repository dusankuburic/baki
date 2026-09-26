// Behaviour of the service worker in public/sw.js, run in a VM context that
// provides only the worker globals it touches.
import {readFileSync} from 'node:fs'
import {join} from 'node:path'
import {runInNewContext} from 'node:vm'
import {afterEach, beforeEach, describe, expect, it, vi, type Mock} from 'vitest'

const ORIGIN = 'http://localhost:8080'
const WORKER_SOURCE = readFileSync(join(process.cwd(), 'public/sw.js'), 'utf8')

type Network = Mock<(req: Request) => Promise<Response>>

function startWorker(network: Network) {
  const listeners = new Map<string, (event: unknown) => void>()
  const cached = new Map<string, Response>()
  const cache = {
    match: async (req: Request) => cached.get(req.url)?.clone(),
    put: async (req: Request, res: Response) => {
      cached.set(req.url, res)
    },
    add: async () => undefined,
  }
  runInNewContext(WORKER_SOURCE, {
    self: {
      location: {origin: ORIGIN},
      addEventListener: (type: string, listener: (event: unknown) => void) => listeners.set(type, listener),
      skipWaiting: async () => undefined,
      clients: {claim: async () => undefined},
    },
    caches: {open: async () => cache, keys: async () => [], delete: async () => true},
    fetch: network,
    Response,
    URL,
    // Resolved per call so fake timers installed by a test apply.
    setTimeout: (fn: () => void, ms: number) => setTimeout(fn, ms),
  })
  const request = (path: string): Promise<Response> => {
    let answer: Promise<Response> | undefined
    listeners.get('fetch')!({
      request: new Request(ORIGIN + path),
      respondWith: (response: Promise<Response>) => (answer = response),
    })
    if (!answer) throw new Error(`the worker did not answer ${path}`)
    return answer
  }
  return {request, cached}
}

const chunk = () => new Response('export const C = 1', {status: 200, headers: {'Content-Type': 'text/javascript'}})
const networkDown = () => new TypeError('Failed to fetch')

describe('service worker: hashed assets', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('retries a chunk whose download failed at the network level', async () => {
    const network: Network = vi.fn()
    network.mockRejectedValueOnce(networkDown()).mockResolvedValue(chunk())
    const worker = startWorker(network)

    const answer = worker.request('/assets/chevron-up-oOu0Hfoj.js')
    await vi.advanceTimersByTimeAsync(250)
    const res = await answer

    expect(res.status).toBe(200)
    expect(await res.text()).toBe('export const C = 1')
    expect(network).toHaveBeenCalledTimes(2)
    expect(worker.cached.has(`${ORIGIN}/assets/chevron-up-oOu0Hfoj.js`)).toBe(true)
  })

  it('answers a network error once the retries run out', async () => {
    const network: Network = vi.fn()
    network.mockRejectedValue(networkDown())
    const answer = startWorker(network).request('/assets/AITab-CWH9-ePf.js')
    await vi.advanceTimersByTimeAsync(1250)

    expect((await answer).type).toBe('error')
    expect(network).toHaveBeenCalledTimes(3)
  })

  it('passes an HTTP error through without retrying or caching it', async () => {
    const network: Network = vi.fn()
    network.mockResolvedValue(new Response('', {status: 404}))
    const worker = startWorker(network)

    expect((await worker.request('/assets/BlockView-removed.js')).status).toBe(404)
    expect(network).toHaveBeenCalledTimes(1)
    expect(worker.cached.size).toBe(0)
  })

  it('serves a cached chunk without the network', async () => {
    const network: Network = vi.fn()
    network.mockResolvedValue(chunk())
    const worker = startWorker(network)
    await (await worker.request('/assets/BlockView-DIpIx-nW.js')).text()

    const again = await worker.request('/assets/BlockView-DIpIx-nW.js')
    expect(await again.text()).toBe('export const C = 1')
    expect(network).toHaveBeenCalledTimes(1)
  })
})

describe('service worker: other same-origin files', () => {
  it('answers a network error when offline with nothing cached', async () => {
    const network: Network = vi.fn()
    network.mockRejectedValue(networkDown())
    expect((await startWorker(network).request('/manifest.json')).type).toBe('error')
  })
})
