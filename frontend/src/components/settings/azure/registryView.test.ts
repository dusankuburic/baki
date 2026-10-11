import {describe, it, expect} from 'vitest'
import type {AzureConnection, AzureDiscovery, AzureModelEntry} from '@/types'
import {
  AZURE_SECTION_ANCHOR,
  discoveryItemKey,
  effectiveDefaultModelId,
  findDisplayNameTwin,
  findDuplicateModel,
  focusAfterConnectionRemoval,
  focusAfterModelRemoval,
  focusKey,
  isChatKind,
  isOwnItem,
  limitsView,
  orgAzureAnchor,
  orgIdFromAnchor,
  sharedGroups,
  unaddedCounts,
  unaddedItems,
} from './registryView'

const USER = {kind: 'user'} as const
const ORG = {kind: 'org', orgId: 'org-1'} as const

function conn(id: string, over: Partial<AzureConnection> = {}): AzureConnection {
  return {
    id,
    name: id,
    shared: false,
    legacy: false,
    config: {endpoint: `https://${id}.openai.azure.com`, authMode: 'api_key', hasSecret: true},
    status: 'ready',
    canDiscover: false,
    supportsAgents: false,
    suggestDiscovery: false,
    modelCount: 0,
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

function model(id: string, over: Partial<AzureModelEntry> = {}): AzureModelEntry {
  return {
    id,
    modelId: `m/${id}`,
    connectionId: 'default',
    connectionName: 'default',
    kind: 'chat',
    deployment: id,
    target: id,
    displayName: id,
    purpose: 'general',
    isDefault: false,
    isKnowledgeEmbedding: false,
    shared: false,
    status: 'ready',
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

describe('registryView anchors', () => {
  it('names the AI Accounts heading and an organisation block, and reads the org back', () => {
    expect(AZURE_SECTION_ANCHOR).toBe('provider-azure-foundry')
    expect(orgAzureAnchor('org-1')).toBe('org-azure-foundry-org-1')
    expect(orgIdFromAnchor(orgAzureAnchor('org-1'))).toBe('org-1')
    expect(orgIdFromAnchor('org-azure-foundry-')).toBeNull()
    expect(orgIdFromAnchor(AZURE_SECTION_ANCHOR)).toBeNull()
    expect(orgIdFromAnchor(null)).toBeNull()
  })
})

describe('registryView ownership and groups', () => {
  it('owns every item in org scope and only the personal ones in user scope', () => {
    expect(isOwnItem(USER, {shared: false})).toBe(true)
    expect(isOwnItem(USER, {shared: true})).toBe(false)
    expect(isOwnItem(ORG, {shared: true})).toBe(true)
    expect(isChatKind('agent')).toBe(true)
    expect(isChatKind('embedding')).toBe(false)
  })

  it('groups the shared entries per organisation, by name, and skips an organisation without models', () => {
    const groups = sharedGroups(
      [
        conn('mine'),
        conn('zres', {shared: true, orgId: 'org-z', orgName: 'Zeta'}),
        conn('ares', {shared: true, orgId: 'org-a', orgName: 'Acme'}),
        conn('empty', {shared: true, orgId: 'org-e', orgName: 'Empty'}),
      ],
      [
        model('own0000000'),
        model('zmodel0000', {shared: true, orgId: 'org-z', orgName: 'Zeta'}),
        model('amodel0000', {shared: true, orgId: 'org-a'}),
        model('amodel0001', {shared: true, orgId: 'org-a', orgName: 'Acme'}),
      ],
    )
    expect(groups.map(g => [g.orgId, g.orgName, g.models.map(m => m.id), g.connections.map(c => c.id)])).toEqual([
      ['org-a', 'Acme', ['amodel0000', 'amodel0001'], ['ares']],
      ['org-z', 'Zeta', ['zmodel0000'], ['zres']],
    ])
    expect(sharedGroups([], [model('x000000000', {shared: true, orgId: 'org-q'})])[0]!.orgName).toBe('org-q')
  })
})

describe('registryView limits', () => {
  it('shows the line from 80 % of either limit and flags the limit itself', () => {
    const limits = {connections: 10, models: 50}
    expect(limitsView(7, 39, limits).show).toBe(false)
    expect(limitsView(8, 0, limits)).toMatchObject({show: true, connections: {used: 8, limit: 10, reached: false}})
    expect(limitsView(0, 40, limits).show).toBe(true)
    expect(limitsView(10, 50, limits)).toMatchObject({connections: {reached: true}, models: {reached: true}})
    expect(limitsView(3, 3, {connections: 0, models: 0})).toMatchObject({
      show: false,
      connections: {reached: false},
    })
  })
})

describe('registryView effective default', () => {
  const models = [model('aaaaaaaaaa'), model('bbbbbbbbbb')]
  it('prefers the saved default, else the server default, when they name a listed entry', () => {
    expect(effectiveDefaultModelId('m/bbbbbbbbbb', 'm/aaaaaaaaaa', models)).toBe('m/bbbbbbbbbb')
    expect(effectiveDefaultModelId('', 'm/aaaaaaaaaa', models)).toBe('m/aaaaaaaaaa')
    expect(effectiveDefaultModelId('gpt-4.1', 'm/aaaaaaaaaa', models)).toBe('m/aaaaaaaaaa')
    expect(effectiveDefaultModelId(undefined, 'm/gone000000', models)).toBeNull()
    expect(effectiveDefaultModelId(null, null, models)).toBeNull()
  })
})

describe('registryView duplicates', () => {
  const models = [
    model('chat000000', {connectionId: 'r1', deployment: 'GPT-4.1'}),
    model('emb0000000', {connectionId: 'r1', kind: 'embedding', deployment: 'embed'}),
    model('agent00000', {connectionId: 'r1', kind: 'agent', deployment: 'helper', displayName: 'Helper'}),
  ]
  it('finds a model of the same resource and name space, case-insensitively, but not itself', () => {
    expect(findDuplicateModel(models, {connectionId: 'r1', kind: 'chat', deployment: ' gpt-4.1 '})?.id).toBe(
      'chat000000',
    )
    expect(findDuplicateModel(models, {connectionId: 'r1', kind: 'claude', deployment: 'gpt-4.1'})?.id).toBe(
      'chat000000',
    )
    expect(findDuplicateModel(models, {connectionId: 'r1', kind: 'embedding', deployment: 'gpt-4.1'})).toBeUndefined()
    expect(findDuplicateModel(models, {connectionId: 'r1', kind: 'chat', deployment: 'helper'})).toBeUndefined()
    expect(findDuplicateModel(models, {connectionId: 'r2', kind: 'chat', deployment: 'gpt-4.1'})).toBeUndefined()
    expect(
      findDuplicateModel(models, {connectionId: 'r1', kind: 'chat', deployment: 'gpt-4.1'}, 'chat000000'),
    ).toBeUndefined()
    expect(findDuplicateModel(models, {connectionId: 'r1', kind: 'chat', deployment: '  '})).toBeUndefined()
  })

  it('finds a display-name twin, trimmed and case-insensitively', () => {
    expect(findDisplayNameTwin(models, ' helper ')?.id).toBe('agent00000')
    expect(findDisplayNameTwin(models, 'Helper', 'agent00000')).toBeUndefined()
    expect(findDisplayNameTwin(models, '')).toBeUndefined()
  })
})

describe('registryView discovery', () => {
  const discovery: AzureDiscovery = {
    deployments: [
      {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: 'm/x'},
      {name: 'o4-mini', kind: 'chat', model: 'o4-mini', alreadyAdded: false},
      {name: 'embed', kind: 'embedding', model: 'text-embedding-3-large', alreadyAdded: false},
    ],
    agents: [{name: 'Helper', kind: 'agent', model: '', alreadyAdded: false}],
    warnings: [],
  }
  it('lists and counts what is not added yet', () => {
    expect(unaddedItems(discovery).map(i => i.name)).toEqual(['o4-mini', 'embed', 'Helper'])
    expect(unaddedCounts(discovery)).toEqual({deployments: 2, agents: 1})
    expect(unaddedItems(null)).toEqual([])
    expect(unaddedCounts(undefined)).toEqual({deployments: 0, agents: 0})
    expect(discoveryItemKey({kind: 'agent', name: 'Helper'})).toBe('agent:helper')
    expect(discoveryItemKey({kind: 'claude', name: 'Sonnet'})).toBe('deployment:sonnet')
  })
})

describe('registryView focus after a removal', () => {
  it("goes to the next row's Edit, else the previous one's, else the resource's Add model", () => {
    expect(focusAfterModelRemoval(['a', 'b', 'c'], 'b', 'r1')).toBe(focusKey.modelEdit('c'))
    expect(focusAfterModelRemoval(['a', 'b', 'c'], 'c', 'r1')).toBe(focusKey.modelEdit('b'))
    expect(focusAfterModelRemoval(['a'], 'a', 'r1')).toBe(focusKey.addModel('r1'))
    expect(focusAfterModelRemoval([], 'a', 'r1')).toBe(focusKey.addModel('r1'))
  })

  it("goes to the next card's heading, else the previous one's, else Add resource", () => {
    expect(focusAfterConnectionRemoval(['r1', 'r2'], 'r1')).toBe(focusKey.connection('r2'))
    expect(focusAfterConnectionRemoval(['r1', 'r2'], 'r2')).toBe(focusKey.connection('r1'))
    expect(focusAfterConnectionRemoval(['r1'], 'r1')).toBe(focusKey.addResource)
  })
})
