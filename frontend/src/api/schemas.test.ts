import {describe, it, expect, vi, beforeEach} from 'vitest'

const state = vi.hoisted(() => ({fail: true}))

function installLazyMock() {
  vi.doMock('./schemas.lazy', () => ({
    buildSchemas: () => {
      if (state.fail) throw new Error('chunk load error')
      return {FindingSchema: {stub: 'finding'}, AppSettingsSchema: {stub: 'settings'}}
    },
  }))
}

describe('api/schemas lazy loading', () => {
  beforeEach(() => {
    vi.resetModules()
    state.fail = true
  })

  it('retries the import after a transient chunk-load failure', async () => {
    installLazyMock()

    const {getFindingSchema} = await import('./schemas')
    await expect(getFindingSchema()).rejects.toThrow('chunk load error')

    state.fail = false
    const schema = await getFindingSchema()
    expect(schema).toBeDefined()
  })

  it('memoizes the successful build (one build, shared references)', async () => {
    state.fail = false
    installLazyMock()

    const {getFindingSchema, getAppSettingsSchema} = await import('./schemas')
    const [a, b, c] = await Promise.all([getFindingSchema(), getFindingSchema(), getAppSettingsSchema()])
    expect(a).toBe(b)
    expect(c).toBeDefined()
  })
})
