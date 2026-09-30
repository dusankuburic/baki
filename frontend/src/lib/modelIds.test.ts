import {describe, it, expect} from 'vitest'
import {
  findModel,
  hasAlias,
  isAgentModel,
  isRegistryModelId,
  modelKind,
  modelNameOf,
  registryEntryId,
  registryModelId,
  resolveAlias,
  storableModelLabel,
} from './modelIds'

describe('registry model ids', () => {
  it('recognises "m/" plus ten lower-case letters or digits, and nothing else', () => {
    expect(isRegistryModelId('m/a1b2c3d4e5')).toBe(true)
    expect(isRegistryModelId('m/A1B2C3D4E5')).toBe(false)
    expect(isRegistryModelId('m/a1b2c3d4e')).toBe(false)
    expect(isRegistryModelId('m/a1b2c3d4e5f')).toBe(false)
    expect(isRegistryModelId('gpt-4.1')).toBe(false)
    expect(isRegistryModelId('agent:m/a1b2c3d4e5')).toBe(false)
    expect(isRegistryModelId('')).toBe(false)
    expect(isRegistryModelId(undefined)).toBe(false)
    expect(isRegistryModelId(null)).toBe(false)
  })

  it('converts between entry ids and chat ids', () => {
    expect(registryModelId('a1b2c3d4e5')).toBe('m/a1b2c3d4e5')
    expect(registryEntryId('m/a1b2c3d4e5')).toBe('a1b2c3d4e5')
    expect(registryEntryId('gpt-4.1')).toBeNull()
    expect(registryEntryId(undefined)).toBeNull()
  })
})

describe('alias resolution', () => {
  const own = {id: 'm/owngpt4100', aliases: ['gpt-4.1', 'agent:helper'], shared: false}
  const shared = {id: 'm/orggpt4100', aliases: ['gpt-4.1', 'claude:sonnet'], shared: true}
  const plain = {id: 'm/plain00000'}

  it('matches aliases case-insensitively and tolerates none', () => {
    expect(hasAlias(own, 'GPT-4.1')).toBe(true)
    expect(hasAlias(own, 'gpt-4o')).toBe(false)
    expect(hasAlias(plain, 'gpt-4.1')).toBe(false)
    expect(hasAlias({id: 'x', aliases: null}, 'gpt-4.1')).toBe(false)
    expect(hasAlias({id: 'x', aliases: []}, 'gpt-4.1')).toBe(false)
  })

  it("prefers the caller's own entry over a shared one, whatever the listing order", () => {
    expect(resolveAlias('gpt-4.1', [shared, own])).toBe(own)
    expect(resolveAlias('gpt-4.1', [own, shared])).toBe(own)
  })

  it('falls back to a shared entry, and to nothing', () => {
    expect(resolveAlias('claude:sonnet', [own, shared])).toBe(shared)
    expect(resolveAlias('agent:unknown', [own, shared])).toBeUndefined()
    expect(resolveAlias('', [own, shared])).toBeUndefined()
  })

  it('finds a listed id first, then an alias', () => {
    const listedRaw = {id: 'gpt-4.1'}
    expect(findModel('gpt-4.1', [own, listedRaw])).toBe(listedRaw)
    expect(findModel('m/plain00000', [own, plain])).toBe(plain)
    expect(findModel('agent:helper', [plain, own])).toBe(own)
    expect(findModel('missing', [own, plain])).toBeUndefined()
    expect(findModel('', [own])).toBeUndefined()
  })
})

describe('model kinds', () => {
  it("takes the listing's kind first", () => {
    expect(modelKind('m/a1b2c3d4e5', {kind: 'agent'})).toBe('agent')
    expect(modelKind('m/a1b2c3d4e5', {kind: 'claude'})).toBe('claude')
    expect(modelKind('m/a1b2c3d4e5', {kind: 'chat'})).toBe('chat')
    expect(modelKind('m/a1b2c3d4e5', {kind: 'embedding'})).toBe('embedding')
    // A kind wins over a misleading legacy prefix.
    expect(modelKind('agent:helper', {kind: 'chat'})).toBe('chat')
  })

  it('falls back to the legacy routing prefixes', () => {
    expect(modelKind('agent:helper')).toBe('agent')
    expect(modelKind('claude:sonnet', null)).toBe('claude')
    // An unknown kind (a newer server) reads like none.
    expect(modelKind('agent:helper', {kind: 'router'})).toBe('agent')
    expect(modelKind('gpt-4.1', {})).toBeUndefined()
    expect(modelKind(undefined)).toBeUndefined()
    expect(modelKind('')).toBeUndefined()
  })

  it('detects agents by kind, then by the legacy prefix', () => {
    expect(isAgentModel('m/a1b2c3d4e5', {kind: 'agent'})).toBe(true)
    expect(isAgentModel('m/a1b2c3d4e5', {kind: 'chat'})).toBe(false)
    expect(isAgentModel('m/a1b2c3d4e5')).toBe(false)
    expect(isAgentModel('agent:helper')).toBe(true)
    expect(isAgentModel('claude:sonnet')).toBe(false)
    expect(isAgentModel(null)).toBe(false)
  })
})

describe('modelNameOf', () => {
  const listed = [
    {id: 'm/docs000000', displayName: 'Docs writer', aliases: ['claude:docs']},
    {id: 'claude-opus-4-1', displayName: 'Claude Opus 4.1'},
  ]

  it('keeps the label a reference was saved with, whatever the listing says now', () => {
    expect(modelNameOf('m/docs000000', 'Docs (old name)', listed)).toBe('Docs (old name)')
  })

  it("takes the listing's display name next, through aliases", () => {
    expect(modelNameOf('m/docs000000', undefined, listed)).toBe('Docs writer')
    expect(modelNameOf('claude:docs', '', listed)).toBe('Docs writer')
    expect(modelNameOf('claude-opus-4-1', null, listed)).toBe('Claude Opus 4.1')
  })

  it('falls back to the id, except a registry id nothing names any more (removed)', () => {
    expect(modelNameOf('gpt-4o', undefined, listed)).toBe('gpt-4o')
    expect(modelNameOf('m/gone000000', undefined, listed)).toBeNull()
    expect(modelNameOf('m/gone000000', undefined)).toBeNull()
    expect(modelNameOf(undefined, undefined, listed)).toBe('')
  })

  // C6: a turn sent before the listing loaded was once saved with its raw
  // id as the label, which then won over the listing's name for good.
  it('treats a label that is only an id as none', () => {
    expect(modelNameOf('m/docs000000', 'm/docs000000', listed)).toBe('Docs writer')
    expect(modelNameOf('claude-opus-4-1', 'claude-opus-4-1', listed)).toBe('Claude Opus 4.1')
    expect(modelNameOf('m/gone000000', 'm/gone000000', listed)).toBeNull()
    expect(modelNameOf('claude:docs', 'm/docs000000', listed)).toBe('Docs writer')
  })
})

describe('storableModelLabel', () => {
  it('keeps a friendly name, never an empty label, the id itself or a registry id', () => {
    expect(storableModelLabel('Docs writer', 'm/docs000000')).toBe('Docs writer')
    expect(storableModelLabel('gpt-4.1', 'm/gpt4100000')).toBe('gpt-4.1')
    expect(storableModelLabel('', 'm/docs000000')).toBeUndefined()
    expect(storableModelLabel('  ', 'm/docs000000')).toBeUndefined()
    expect(storableModelLabel(undefined, 'm/docs000000')).toBeUndefined()
    expect(storableModelLabel('claude-custom', 'claude-custom')).toBeUndefined()
    expect(storableModelLabel('m/docs000000', 'm/docs000000')).toBeUndefined()
    expect(storableModelLabel('m/docs000000', 'gpt-4.1')).toBeUndefined()
  })
})
