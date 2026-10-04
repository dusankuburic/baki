import {describe, it, expect, vi, beforeEach} from 'vitest'

// The entry module's boot-time side effects: the deep-link view, and the
// warm-up of the chunks the first screen needs (HomeDashboard, BlockView).
// Rendering itself is stubbed — createRoot is mocked, App is a no-op — so
// the test sees only what main.tsx does on its own.
const boot = vi.hoisted(() => ({
  // Factory invocations, in order: a module's factory runs when its chunk
  // is first imported, which is exactly the warm-up this guards.
  warmed: [] as string[],
  render: vi.fn(),
}))

vi.mock('react-dom/client', () => ({
  createRoot: () => ({render: boot.render, unmount: vi.fn()}),
}))
vi.mock('./App', () => ({default: () => null}))
vi.mock('./components/auth/ProtectedRoute', () => ({
  default: ({children}: {children: React.ReactNode}) => children,
}))
vi.mock('./components/shared/SharedReportView', () => ({default: () => null}))

async function bootAt(path: string) {
  window.history.replaceState({}, '', path)
  vi.resetModules()
  // Registered per boot: a hoisted vi.mock's instance survives resetModules,
  // so its factory would record only the first import across the file.
  vi.doMock('@/components/dashboard/HomeDashboard', () => {
    boot.warmed.push('HomeDashboard')
    return {default: () => null}
  })
  vi.doMock('@/components/flow/BlockView', () => {
    boot.warmed.push('BlockView')
    return {default: () => null}
  })
  await import('./main')
  // The warm-up is fire-and-forget; let the import promises settle.
  await new Promise(r => setTimeout(r, 0))
}

beforeEach(() => {
  boot.warmed.length = 0
  boot.render.mockClear()
  document.body.innerHTML = '<div id="root"></div>'
})

describe('main.tsx boot', () => {
  it('warms the Home and BlockView chunks when the shell opens on Home', async () => {
    await bootAt('/')
    expect(boot.render).toHaveBeenCalledTimes(1)
    expect(boot.warmed).toEqual(expect.arrayContaining(['HomeDashboard', 'BlockView']))
    expect(boot.warmed).toHaveLength(2)
  })

  it('skips the Home chunk when a ?view= deep link opens another view', async () => {
    await bootAt('/?view=library')
    expect(boot.render).toHaveBeenCalledTimes(1)
    expect(boot.warmed).toEqual(['BlockView'])
  })

  it('warms nothing for the public /shared viewer', async () => {
    await bootAt('/shared')
    expect(boot.render).toHaveBeenCalledTimes(1)
    expect(boot.warmed).toEqual([])
  })
})
