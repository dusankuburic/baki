import {describe, it, expect} from 'vitest'
import {
  legacyDefaultRewrite,
  providerNewChatDefault,
  resolveThreadModel,
  type ModelSelectionInput,
  type SelectableModel,
  type SelectableProvider,
} from './modelSelection'

// Azure registry entries (the migrated `default` resource's carry aliases).
const gpt: SelectableModel = {id: 'm/gpt4100000', displayName: 'GPT-4.1', kind: 'chat', aliases: ['gpt-4.1']}
const docs: SelectableModel = {id: 'm/docs000000', displayName: 'Docs writer', kind: 'claude', aliases: ['claude:docs']}
const tester: SelectableModel = {
  id: 'm/tester0000',
  displayName: 'Tester',
  kind: 'chat',
  status: 'needs_credentials',
  statusReason: "Its resource's credentials must be entered again",
}
const embed: SelectableModel = {id: 'm/embed00000', displayName: 'Search index', kind: 'embedding'}
const sharedReview: SelectableModel = {
  id: 'm/review0000',
  displayName: 'Review bot',
  kind: 'agent',
  shared: true,
  aliases: ['agent:review'],
}

function azure(models: SelectableModel[], over: Partial<SelectableProvider> = {}): SelectableProvider {
  return {id: 'azure-foundry', configured: true, defaultModel: models[0]?.id ?? '', models, ...over}
}

const claude: SelectableProvider = {
  id: 'claude',
  configured: true,
  defaultModel: 'claude-sonnet-4-6',
  models: [
    {id: 'claude-sonnet-4-6', displayName: 'Claude Sonnet 4.6'},
    {id: 'claude-opus-4-1', displayName: 'Claude Opus 4.1'},
  ],
}

function input(over: Partial<ModelSelectionInput> = {}): ModelSelectionInput {
  return {
    settings: {activeProvider: 'azure-foundry', providers: {}},
    providers: [azure([gpt, docs, tester, embed]), claude],
    listingLoaded: true,
    ...over,
  }
}

const withDefaults = (active: string, defaults: Record<string, string>) => ({
  activeProvider: active,
  providers: Object.fromEntries(Object.entries(defaults).map(([p, m]) => [p, {defaultModel: m}])),
})

describe('resolveThreadModel: a pinned thread', () => {
  it('keeps its pin', () => {
    const sel = resolveThreadModel(input({pin: {provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'}}))
    expect(sel).toEqual({provider: 'azure-foundry', model: docs.id, label: 'Docs writer', reason: 'pinned'})
  })

  it('refreshes a renamed or unlabelled pin', () => {
    const renamed = resolveThreadModel(
      input({pin: {provider: 'azure-foundry', model: docs.id, modelLabel: 'Old name'}}),
    )
    expect(renamed).toMatchObject({label: 'Docs writer', reason: 'pinned'})
    expect(renamed.pin).toEqual({provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'})
    const unlabelled = resolveThreadModel(input({pin: {provider: 'claude', model: 'claude-opus-4-1'}}))
    expect(unlabelled.pin).toEqual({provider: 'claude', model: 'claude-opus-4-1', modelLabel: 'Claude Opus 4.1'})
  })

  it('moves a pinned pre-registry id onto its entry', () => {
    const sel = resolveThreadModel(
      input({pin: {provider: 'azure-foundry', model: 'claude:docs', modelLabel: 'Docs writer'}}),
    )
    expect(sel).toMatchObject({model: docs.id, reason: 'pinned'})
    expect(sel.pin).toEqual({provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'})
  })

  it('keeps a catalogue model the catalogue does not list (a custom id that worked)', () => {
    const sel = resolveThreadModel(input({pin: {provider: 'claude', model: 'claude-custom-preview'}}))
    expect(sel).toEqual({
      provider: 'claude',
      model: 'claude-custom-preview',
      label: 'claude-custom-preview',
      reason: 'pinned',
    })
  })

  it('trusts the pin while the listing loads or after it failed — no notice', () => {
    const pin = {provider: 'azure-foundry', model: 'm/removed000', modelLabel: 'Gone'}
    expect(resolveThreadModel(input({pin, listingLoaded: false}))).toEqual({
      provider: 'azure-foundry',
      model: 'm/removed000',
      label: 'Gone',
      reason: 'pinned',
    })
  })

  it('treats an incomplete pin as none', () => {
    expect(resolveThreadModel(input({pin: {provider: 'azure-foundry', model: ''}})).reason).toBe('serverDefault')
    expect(resolveThreadModel(input({pin: {model: docs.id}})).reason).toBe('serverDefault')
  })
})

describe('resolveThreadModel: fallbacks', () => {
  it("falls back to its provider's default when the model was removed, with the notice data", () => {
    const sel = resolveThreadModel(
      input({
        pin: {provider: 'azure-foundry', model: 'm/removed000', modelLabel: 'Old writer'},
        settings: withDefaults('claude', {'azure-foundry': docs.id}),
      }),
    )
    expect(sel).toEqual({
      provider: 'azure-foundry',
      model: docs.id,
      label: 'Docs writer',
      reason: 'fallbackRemoved',
      notice: {
        kind: 'removed',
        wanted: {provider: 'azure-foundry', model: 'm/removed000', label: 'Old writer'},
        fallback: {provider: 'azure-foundry', model: docs.id, label: 'Docs writer'},
      },
    })
    // Dismissing pins the fallback; nothing is pinned automatically.
    expect(sel.pin).toBeUndefined()
  })

  it('never re-pins a model whose resource needs credentials, and passes the reason on', () => {
    const sel = resolveThreadModel(input({pin: {provider: 'azure-foundry', model: tester.id, modelLabel: 'Tester'}}))
    expect(sel).toMatchObject({provider: 'azure-foundry', model: gpt.id, reason: 'fallbackUnavailable'})
    expect(sel.pin).toBeUndefined()
    expect(sel.notice).toEqual({
      kind: 'unavailable',
      wanted: {provider: 'azure-foundry', model: tester.id, label: 'Tester'},
      fallback: {provider: 'azure-foundry', model: gpt.id, label: 'GPT-4.1'},
      cause: 'needs_credentials',
      statusReason: tester.statusReason,
    })
  })

  it('reports an unusable status without copy as just the cause', () => {
    const odd = {...docs, status: 'unsupported' as const}
    const sel = resolveThreadModel(
      input({pin: {provider: 'azure-foundry', model: docs.id}, providers: [azure([gpt, odd]), claude]}),
    )
    expect(sel.notice).toMatchObject({kind: 'unavailable', cause: 'unsupported'})
    expect(sel.notice).not.toHaveProperty('statusReason')
  })

  it('is unavailable when its provider is not configured here, and falls back to the new-chat default', () => {
    const sel = resolveThreadModel(
      input({
        pin: {provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'},
        providers: [azure([docs], {configured: false}), claude],
        settings: withDefaults('azure-foundry', {}),
      }),
    )
    expect(sel).toMatchObject({provider: 'claude', model: 'claude-sonnet-4-6', reason: 'fallbackUnavailable'})
    expect(sel.notice).toMatchObject({kind: 'unavailable', cause: 'provider'})
  })

  it('falls back across providers when its own offers nothing usable, and to nothing when no one does', () => {
    const onlyTester = azure([tester, embed], {defaultModel: tester.id})
    const toClaude = resolveThreadModel(
      input({
        pin: {provider: 'azure-foundry', model: tester.id},
        providers: [onlyTester, claude],
        settings: withDefaults('claude', {}),
      }),
    )
    expect(toClaude).toMatchObject({provider: 'claude', model: 'claude-sonnet-4-6', reason: 'fallbackUnavailable'})

    const nothing = resolveThreadModel(
      input({pin: {provider: 'azure-foundry', model: tester.id, modelLabel: 'Tester'}, providers: [onlyTester]}),
    )
    expect(nothing).toEqual({
      provider: 'azure-foundry',
      model: '',
      label: '',
      reason: 'fallbackUnavailable',
      notice: {
        kind: 'unavailable',
        wanted: {provider: 'azure-foundry', model: tester.id, label: 'Tester'},
        fallback: null,
        cause: 'needs_credentials',
        statusReason: tester.statusReason,
      },
    })
  })

  it('never selects an embedding entry, even when a pin names one', () => {
    const sel = resolveThreadModel(input({pin: {provider: 'azure-foundry', model: embed.id}}))
    expect(sel).toMatchObject({model: gpt.id, reason: 'fallbackRemoved'})
  })

  it('removes a local model the local server stopped serving', () => {
    const local: SelectableProvider = {
      id: 'local',
      configured: true,
      defaultModel: 'qwen2.5:7b',
      models: [{id: 'qwen2.5:7b'}],
    }
    const sel = resolveThreadModel(input({pin: {provider: 'local', model: 'phi-4-mini'}, providers: [local]}))
    expect(sel).toMatchObject({provider: 'local', model: 'qwen2.5:7b', label: 'qwen2.5:7b', reason: 'fallbackRemoved'})
  })
})

describe('resolveThreadModel: a thread from before per-thread memory (legacy)', () => {
  const reply = (provider: string, model: string, extra: object = {}) => ({
    role: 'assistant',
    provider,
    model,
    ...extra,
  })

  it("pins itself to its last assistant message's pair, through aliases", () => {
    const messages = [
      {role: 'user'},
      reply('azure-foundry', 'gpt-4.1'),
      {role: 'user', provider: 'azure-foundry', model: 'ignored'},
      reply('azure-foundry', 'claude:docs'),
      {role: 'assistant'}, // no pair (an interrupted turn): skipped
    ]
    const sel = resolveThreadModel(input({messages}))
    expect(sel).toEqual({
      provider: 'azure-foundry',
      model: docs.id,
      label: 'Docs writer',
      reason: 'legacy',
      pin: {provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'},
    })
  })

  it('starts like a new thread when no assistant message names a pair', () => {
    const sel = resolveThreadModel(input({messages: [{role: 'user'}, {role: 'assistant', provider: 'claude'}]}))
    expect(sel).toEqual({provider: 'azure-foundry', model: gpt.id, label: 'GPT-4.1', reason: 'serverDefault'})
  })

  it('takes the pair as it is while the listing is not loaded, pinning nothing', () => {
    const sel = resolveThreadModel(
      input({messages: [reply('azure-foundry', 'gpt-4.1', {modelLabel: 'GPT'})], listingLoaded: false}),
    )
    expect(sel).toEqual({provider: 'azure-foundry', model: 'gpt-4.1', label: 'GPT', reason: 'legacy'})
  })

  it('follows the default with a removal notice when the pair is gone', () => {
    const sel = resolveThreadModel(input({messages: [reply('azure-foundry', 'agent:old')]}))
    expect(sel).toMatchObject({provider: 'azure-foundry', model: gpt.id, reason: 'fallbackRemoved'})
    expect(sel.notice?.wanted).toEqual({provider: 'azure-foundry', model: 'agent:old', label: 'agent:old'})
    expect(sel.pin).toBeUndefined()
  })

  it('shows the unavailable notice when the pair needs credentials', () => {
    const sel = resolveThreadModel(input({messages: [reply('azure-foundry', tester.id, {modelLabel: 'Tester'})]}))
    expect(sel).toMatchObject({reason: 'fallbackUnavailable', notice: {cause: 'needs_credentials'}})
  })

  it("keeps a catalogue pair the catalogue lists, or the provider's own defaults", () => {
    expect(resolveThreadModel(input({messages: [reply('claude', 'claude-opus-4-1')]}))).toMatchObject({
      model: 'claude-opus-4-1',
      reason: 'legacy',
      pin: {provider: 'claude', model: 'claude-opus-4-1', modelLabel: 'Claude Opus 4.1'},
    })
    const custom = resolveThreadModel(
      input({
        messages: [reply('claude', 'claude-custom')],
        settings: withDefaults('claude', {claude: 'claude-custom'}),
      }),
    )
    // Its pin stores no label: one that is only the id adds nothing (C6).
    expect(custom).toEqual({
      provider: 'claude',
      model: 'claude-custom',
      label: 'claude-custom',
      reason: 'legacy',
      pin: {provider: 'claude', model: 'claude-custom'},
    })
    const serverDefault = resolveThreadModel(
      input({messages: [reply('claude', 'claude-next')], providers: [{...claude, defaultModel: 'claude-next'}]}),
    )
    expect(serverDefault).toMatchObject({model: 'claude-next', reason: 'legacy'})
  })

  it('counts a catalogue pair the provider neither lists nor defaults to as removed', () => {
    const sel = resolveThreadModel(
      input({messages: [reply('claude', 'claude-3-opus')], settings: withDefaults('claude', {})}),
    )
    expect(sel).toMatchObject({provider: 'claude', model: 'claude-sonnet-4-6', reason: 'fallbackRemoved'})
  })

  // C1: unpinned, the stand-in's first reply became the thread's "last
  // reply", and inference then adopted the stand-in for good.
  describe('whose model is only temporarily unavailable', () => {
    const legacyGpt: SelectableModel = {
      id: 'm/legacy0000',
      displayName: 'Old GPT',
      kind: 'chat',
      aliases: ['gpt-4.1'],
      status: 'needs_credentials',
    }
    const gpt4o: SelectableModel = {id: 'm/gpt4o00000', displayName: 'GPT-4o', kind: 'chat'}
    const providers = [azure([legacyGpt, gpt4o], {defaultModel: gpt4o.id})]
    const history = [{role: 'user'}, reply('azure-foundry', 'gpt-4.1', {modelLabel: 'GPT-4.1 (2025)'})]

    it('pins itself to its own model (through aliases), uses the stand-in meanwhile, and keeps the notice', () => {
      const sel = resolveThreadModel(input({messages: history, providers}))
      expect(sel).toMatchObject({
        provider: 'azure-foundry',
        model: gpt4o.id,
        reason: 'fallbackUnavailable',
        notice: {
          kind: 'unavailable',
          cause: 'needs_credentials',
          wanted: {model: 'gpt-4.1', label: 'GPT-4.1 (2025)'},
          fallback: {model: gpt4o.id},
        },
      })
      expect(sel.pin).toEqual({provider: 'azure-foundry', model: legacyGpt.id, modelLabel: 'GPT-4.1 (2025)'})
    })

    it('a reply from the stand-in does not move it: it returns to its model once that can be used', () => {
      const pin = resolveThreadModel(input({messages: history, providers})).pin
      const afterReply = [
        ...history,
        {role: 'user'},
        // The turn sent with pinThread:false went to the stand-in.
        reply('azure-foundry', gpt4o.id, {modelLabel: 'GPT-4o'}),
      ]
      const sel = resolveThreadModel(input({pin, messages: afterReply, providers}))
      expect(sel).toMatchObject({model: gpt4o.id, reason: 'fallbackUnavailable', notice: {kind: 'unavailable'}})
      expect(sel.pin).toBeUndefined()

      const fixed = [azure([{...legacyGpt, status: 'ready'}, gpt4o], {defaultModel: gpt4o.id})]
      expect(resolveThreadModel(input({pin, messages: afterReply, providers: fixed}))).toMatchObject({
        model: legacyGpt.id,
        label: 'Old GPT',
        reason: 'pinned',
      })
    })

    it('names its entry when the reply stored no name of its own', () => {
      const sel = resolveThreadModel(input({messages: [reply('azure-foundry', legacyGpt.id)], providers}))
      expect(sel.notice?.wanted).toEqual({provider: 'azure-foundry', model: legacyGpt.id, label: 'Old GPT'})
      expect(sel.pin).toEqual({provider: 'azure-foundry', model: legacyGpt.id, modelLabel: 'Old GPT'})
    })

    it("pins itself too when its provider isn't configured here", () => {
      const sel = resolveThreadModel(
        input({messages: [reply('openai', 'gpt-5', {modelLabel: 'GPT-5'})], settings: withDefaults('claude', {})}),
      )
      expect(sel).toMatchObject({provider: 'claude', reason: 'fallbackUnavailable', notice: {cause: 'provider'}})
      expect(sel.pin).toEqual({provider: 'openai', model: 'gpt-5', modelLabel: 'GPT-5'})
    })
  })
})

// C2: a configured provider can list nothing for a while — a local server
// that is down, an Azure AI Foundry scope whose legacy import is still
// running. That says nothing about the thread's model.
describe('resolveThreadModel: a configured provider that lists nothing to chat with', () => {
  const localDown: SelectableProvider = {id: 'local', configured: true, defaultModel: '', models: []}

  it('keeps a thread pinned to a local model while the local server is down — no notice, no move to the cloud', () => {
    const sel = resolveThreadModel(
      input({
        pin: {provider: 'local', model: 'llama3.1:8b', modelLabel: 'Llama 3.1 8B'},
        settings: withDefaults('claude', {claude: 'claude-sonnet-4-6'}),
        providers: [localDown, claude],
      }),
    )
    expect(sel).toEqual({provider: 'local', model: 'llama3.1:8b', label: 'Llama 3.1 8B', reason: 'pinned'})
  })

  it('keeps an Azure pin while Azure is configured with no models (the legacy import is pending)', () => {
    const pending = azure([], {defaultModel: ''})
    const pinned = resolveThreadModel(
      input({
        pin: {provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'},
        providers: [pending, claude],
      }),
    )
    expect(pinned).toEqual({provider: 'azure-foundry', model: docs.id, label: 'Docs writer', reason: 'pinned'})
    // Only an embedding model listed counts as nothing to chat with, too.
    const embedOnly = resolveThreadModel(
      input({pin: {provider: 'azure-foundry', model: docs.id}, providers: [azure([embed]), claude]}),
    )
    expect(embedOnly).toMatchObject({model: docs.id, reason: 'pinned'})
    expect(embedOnly.notice).toBeUndefined()
  })

  it('takes a legacy pair as it is, pinning nothing', () => {
    const sel = resolveThreadModel(
      input({
        messages: [{role: 'user'}, {role: 'assistant', provider: 'local', model: 'llama3.1:8b'}],
        providers: [localDown, claude],
      }),
    )
    expect(sel).toEqual({provider: 'local', model: 'llama3.1:8b', label: 'llama3.1:8b', reason: 'legacy'})
  })

  it('starts a new thread on it with no model rather than one it made up', () => {
    const sel = resolveThreadModel(
      input({settings: withDefaults('local', {local: 'llama3.1:8b'}), providers: [localDown, claude]}),
    )
    expect(sel).toEqual({provider: 'local', model: '', label: '', reason: 'serverDefault'})
    expect(providerNewChatDefault(withDefaults('local', {local: 'llama3.1:8b'}), localDown)).toBeNull()
  })
})

// C6: before the listing loads, a registry id has no name yet — and a turn
// sent meanwhile stored "m/…" as its label, which then won for good.
describe('resolveThreadModel: names before the listing loads', () => {
  it("never shows a registry id as a new thread's or a pin's name", () => {
    const loading = {listingLoaded: false, settings: withDefaults('azure-foundry', {'azure-foundry': docs.id})}
    expect(resolveThreadModel(input(loading))).toEqual({
      provider: 'azure-foundry',
      model: docs.id,
      label: '',
      reason: 'default',
    })
    expect(resolveThreadModel(input({...loading, pin: {provider: 'azure-foundry', model: docs.id}}))).toEqual({
      provider: 'azure-foundry',
      model: docs.id,
      label: '',
      reason: 'pinned',
    })
  })

  it('names the pin once the listing loads, storing the name', () => {
    const sel = resolveThreadModel(input({pin: {provider: 'azure-foundry', model: docs.id}}))
    expect(sel).toMatchObject({label: 'Docs writer', reason: 'pinned'})
    expect(sel.pin).toEqual({provider: 'azure-foundry', model: docs.id, modelLabel: 'Docs writer'})
  })
})

describe('resolveThreadModel: a new thread', () => {
  it("starts on the active provider's Default for new chats, resolved through aliases", () => {
    expect(resolveThreadModel(input({settings: withDefaults('azure-foundry', {'azure-foundry': docs.id})}))).toEqual({
      provider: 'azure-foundry',
      model: docs.id,
      label: 'Docs writer',
      reason: 'default',
    })
    expect(
      resolveThreadModel(input({settings: withDefaults('azure-foundry', {'azure-foundry': ' gpt-4.1 '})})),
    ).toMatchObject({model: gpt.id, reason: 'default'})
  })

  it('uses the server default when none is saved, or the saved one is gone or unusable', () => {
    expect(resolveThreadModel(input())).toEqual({
      provider: 'azure-foundry',
      model: gpt.id,
      label: 'GPT-4.1',
      reason: 'serverDefault',
    })
    expect(
      resolveThreadModel(input({settings: withDefaults('azure-foundry', {'azure-foundry': 'm/removed000'})})),
    ).toMatchObject({model: gpt.id, reason: 'serverDefault'})
    expect(
      resolveThreadModel(input({settings: withDefaults('azure-foundry', {'azure-foundry': tester.id})})),
    ).toMatchObject({model: gpt.id, reason: 'serverDefault'})
  })

  it('takes the first usable entry when the server default is gone or unusable', () => {
    const gone = azure([tester, docs], {defaultModel: 'm/removed000'})
    expect(resolveThreadModel(input({providers: [gone]}))).toMatchObject({model: docs.id, reason: 'serverDefault'})
    const unusable = azure([tester, docs], {defaultModel: tester.id})
    expect(resolveThreadModel(input({providers: [unusable]}))).toMatchObject({model: docs.id, reason: 'serverDefault'})
  })

  it("serves a catalogue provider's unlisted saved or server default as is", () => {
    expect(resolveThreadModel(input({settings: withDefaults('claude', {claude: 'claude-custom'})}))).toEqual({
      provider: 'claude',
      model: 'claude-custom',
      label: 'claude-custom',
      reason: 'default',
    })
    const unlistedDefault = {...claude, defaultModel: 'claude-next', models: []}
    expect(resolveThreadModel(input({settings: withDefaults('claude', {}), providers: [unlistedDefault]}))).toEqual({
      provider: 'claude',
      model: 'claude-next',
      label: 'claude-next',
      reason: 'serverDefault',
    })
  })

  it('stays on a configured active provider that offers nothing usable (the composer explains)', () => {
    const sel = resolveThreadModel(input({providers: [azure([tester, embed], {defaultModel: ''}), claude]}))
    expect(sel).toEqual({provider: 'azure-foundry', model: '', label: '', reason: 'serverDefault'})
  })

  it('uses the first configured provider when the active one is not configured here, writing nothing', () => {
    const sel = resolveThreadModel(
      input({
        settings: withDefaults('gemini', {claude: 'claude-opus-4-1'}),
        providers: [azure([gpt], {configured: false}), claude],
      }),
    )
    expect(sel).toEqual({provider: 'claude', model: 'claude-opus-4-1', label: 'Claude Opus 4.1', reason: 'default'})
  })

  it('names no model when no provider is configured', () => {
    const sel = resolveThreadModel(
      input({providers: [azure([gpt], {configured: false})], settings: withDefaults('claude', {})}),
    )
    expect(sel).toEqual({provider: 'claude', model: '', label: '', reason: 'serverDefault'})
  })

  it('uses the saved default, or the server default, as is while the listing loads', () => {
    expect(
      resolveThreadModel(
        input({listingLoaded: false, settings: withDefaults('azure-foundry', {'azure-foundry': 'gpt-4.1'})}),
      ),
    ).toEqual({provider: 'azure-foundry', model: 'gpt-4.1', label: 'gpt-4.1', reason: 'default'})
    expect(resolveThreadModel(input({listingLoaded: false}))).toEqual({
      provider: 'azure-foundry',
      model: '',
      label: '',
      reason: 'serverDefault',
    })
  })
})

describe('legacyDefaultRewrite', () => {
  const listing = [gpt, docs, sharedReview, embed, {id: 'gpt-4o'}]

  it("rewrites a pre-registry default the caller's own entry answers for", () => {
    expect(legacyDefaultRewrite('gpt-4.1', listing, true)).toEqual({rewriteTo: gpt.id, unresolved: false})
    expect(legacyDefaultRewrite(' CLAUDE:docs ', listing, true)).toEqual({rewriteTo: docs.id, unresolved: false})
  })

  it("leaves one only a shared entry answers for (the org's aliases can change)", () => {
    expect(legacyDefaultRewrite('agent:review', listing, true)).toEqual({rewriteTo: null, unresolved: false})
  })

  it('flags one nothing answers for, once the listing loaded', () => {
    expect(legacyDefaultRewrite('agent:gone', listing, true)).toEqual({rewriteTo: null, unresolved: true})
    expect(legacyDefaultRewrite('agent:gone', listing, false)).toEqual({rewriteTo: null, unresolved: false})
  })

  it('needs nothing for a registry id, an id listed as is, or no default', () => {
    const none = {rewriteTo: null, unresolved: false}
    expect(legacyDefaultRewrite(gpt.id, listing, true)).toEqual(none)
    expect(legacyDefaultRewrite('m/removed000', listing, true)).toEqual(none)
    expect(legacyDefaultRewrite('gpt-4o', listing, true)).toEqual(none)
    expect(legacyDefaultRewrite('', listing, true)).toEqual(none)
    expect(legacyDefaultRewrite(undefined, listing, true)).toEqual(none)
  })

  // C2: Azure lists nothing while its legacy import is pending — the saved
  // default is not "gone" then.
  it('decides nothing on a listing with nothing to chat with', () => {
    const none = {rewriteTo: null, unresolved: false}
    expect(legacyDefaultRewrite('gpt-4.1', [], true)).toEqual(none)
    expect(legacyDefaultRewrite('agent:gone', [embed], true)).toEqual(none)
  })

  it('never rewrites onto an embedding entry', () => {
    const embedWithAlias = {...embed, aliases: ['text-embedding-3-large']}
    expect(legacyDefaultRewrite('text-embedding-3-large', [gpt, embedWithAlias], true)).toEqual({
      rewriteTo: null,
      unresolved: true,
    })
  })
})

describe('providerNewChatDefault', () => {
  it("is the model the provider's new chats start on here — the picker's [DEFAULT]", () => {
    // The saved default, resolved through aliases…
    expect(
      providerNewChatDefault(withDefaults('azure-foundry', {'azure-foundry': 'claude:docs'}), azure([gpt, docs])),
    ).toMatchObject({model: docs.id, label: 'Docs writer', reason: 'default'})
    // …else the server default…
    expect(providerNewChatDefault({activeProvider: 'claude', providers: {}}, azure([gpt, docs]))).toMatchObject({
      model: gpt.id,
      reason: 'serverDefault',
    })
    // …never one that cannot be used, and null when nothing can.
    expect(
      providerNewChatDefault(
        withDefaults('azure-foundry', {'azure-foundry': tester.id}),
        azure([tester, gpt], {defaultModel: tester.id}),
      ),
    ).toMatchObject({model: gpt.id})
    expect(providerNewChatDefault({activeProvider: 'claude', providers: {}}, azure([tester]))).toBeNull()
  })
})
