import {describe, it, expect, vi, beforeEach} from 'vitest'

const boot = vi.hoisted(() => ({
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
  vi.doMock('@/components/dashboard/HomeDashboard', () => {
    boot.warmed.push('HomeDashboard')
    return {default: () => null}
  })
  vi.doMock('@/components/flow/BlockView', () => {
    boot.warmed.push('BlockView')
    return {default: () => null}
  })
  await import('./main')
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
