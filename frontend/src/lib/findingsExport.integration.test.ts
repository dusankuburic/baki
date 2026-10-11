import {describe, it, expect, vi, afterEach} from 'vitest'

const fileSave = vi.fn()
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
    fileSave,
  }),
}))

let nativeDialogs = false
vi.mock('@/platform/guards', () => ({
  getPlatformCapabilities: () => ({nativeDialogs}),
}))

const downloadBlob = vi.fn()
vi.mock('@/lib/csv', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/csv')>()),
  downloadBlob,
}))

const junit =
  '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites name="PAD Analysis" tests="2" failures="1"></testsuites>\n'
const sarif = '{\n  "version": "2.1.0",\n  "runs": [{"results": [{"message": {"text": "\\u003cscript\\u003e"}}]}]\n}\n'

function serve() {
  return vi.spyOn(globalThis, 'fetch').mockImplementation(async input => {
    const url = String(input)
    if (url.endsWith('/api/analysis/export/junit')) {
      return new Response(junit, {status: 200, headers: {'Content-Type': 'application/xml; charset=utf-8'}})
    }
    if (url.endsWith('/api/analysis/export/sarif')) {
      return new Response(sarif, {status: 200, headers: {'Content-Type': 'application/sarif+json'}})
    }
    return new Response('{"status":"ok"}', {status: 200, headers: {'Content-Type': 'application/json'}})
  })
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.clearAllMocks()
  nativeDialogs = false
})

describe('findings exports through the API client', () => {
  it.each([
    ['JUnit', 'junit', junit, 'application/xml;charset=utf-8;', 'xml'],
    ['SARIF', 'sarif', sarif, 'application/sarif+json;charset=utf-8;', 'sarif'],
  ] as const)('%s on the web downloads the server bytes', async (_label, route, body, mime, ext) => {
    const fetchSpy = serve()
    const m = await import('./findingsExport')
    await (route === 'junit' ? m.exportFindingsJUnit('doc1') : m.exportFindingsSARIF('doc1'))

    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`http://localhost:9999/api/analysis/export/${route}`)
    expect(init.method).toBe('POST')
    expect(downloadBlob).toHaveBeenCalledTimes(1)
    const [content, gotMime, filename] = downloadBlob.mock.calls[0]!
    expect(content).toBe(body)
    expect(gotMime).toBe(mime)
    expect(filename).toMatch(new RegExp(`^analysis-doc1-\\d{4}-\\d{2}-\\d{2}\\.${ext}$`))
  })

  it.each([
    ['JUnit', 'junit', junit],
    ['SARIF', 'sarif', sarif],
  ] as const)('%s on the desktop hands the server bytes to /api/export/save', async (_label, route, body) => {
    nativeDialogs = true
    fileSave.mockResolvedValue('C:\\exports\\findings')
    const fetchSpy = serve()
    const m = await import('./findingsExport')
    await (route === 'junit' ? m.exportFindingsJUnit('doc1') : m.exportFindingsSARIF('doc1'))

    expect(downloadBlob).not.toHaveBeenCalled()
    const save = fetchSpy.mock.calls.find(([u]) => String(u).endsWith('/api/export/save'))
    expect(save).toBeDefined()
    expect(JSON.parse(String((save![1] as RequestInit).body))).toEqual({path: 'C:\\exports\\findings', content: body})
  })
})
