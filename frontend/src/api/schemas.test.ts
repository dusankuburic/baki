import {describe, it, expect, vi, beforeEach} from 'vitest'

// The lazy schema module memoizes the dynamic zod import. Its failure path
// must NOT memoize: a transient chunk-load error (deploy invalidates hashed
// chunk names under an open tab) has to retry on the next call, or every
// validated endpoint (login/me/refresh/analysis/settings/flow:loaded) stays
// broken for the whole session.
const state = vi.hoisted(() => ({fail: true}))

function installLazyMock() {
  vi.doMock('./schemas.lazy', () => ({
    // Stateful stand-in for the real builder: throws while state.fail is set
    // (simulating a failed dynamic import / broken module), succeeds after.
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
