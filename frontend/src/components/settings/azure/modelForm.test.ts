import {describe, it, expect} from 'vitest'
import type {AzureDiscoveryItem, AzureModelEntry} from '@/types'
import {unknownValue} from '@/testing/testHelpers'
import {
  cleanLength,
  discoveryQuickAddInput,
  emptyModelForm,
  inferKind,
  modelErrorTarget,
  modelFormFromDiscovery,
  modelFormFromEntry,
  modelFormsDiffer,
  toModelInput,
  toModelUpdateInput,
  validateModelForm,
  withDeployment,
  withDisplayName,
  type ModelFormState,
} from './modelForm'

function entry(over: Partial<AzureModelEntry> = {}): AzureModelEntry {
  return {
    id: 'docs000000',
    modelId: 'm/docs000000',
    connectionId: 'abc0123456',
    connectionName: 'contoso-prod',
    kind: 'claude',
    deployment: 'claude-sonnet-4-6',
    target: 'claude:claude-sonnet-4-6',
    displayName: 'Docs writer',
    purpose: 'documentation',
    isDefault: false,
    isKnowledgeEmbedding: false,
    shared: false,
    status: 'ready',
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

const form = (over: Partial<ModelFormState> = {}): ModelFormState => ({
  ...emptyModelForm('abc0123456'),
  deployment: 'gpt-4.1',
  displayName: 'gpt-4.1',
  ...over,
})

describe('the form', () => {
  it('starts as a General chat deployment on the given resource', () => {
    expect(emptyModelForm()).toEqual({
      connectionId: '',
      kind: 'chat',
      deployment: '',
      displayName: '',
      displayNameEdited: false,
      purpose: 'general',
      purposeCustom: '',
      description: '',
      contextWindow: '',
      maxOutputTokens: '',
    })
    expect(emptyModelForm('abc0123456').connectionId).toBe('abc0123456')
  })

  it('edits a saved entry, custom purpose and overrides included', () => {
    expect(
      modelFormFromEntry(entry({description: 'Writes docs', contextWindow: 200000, maxOutputTokens: 8192})),
    ).toEqual({
      connectionId: 'abc0123456',
      kind: 'claude',
      deployment: 'claude-sonnet-4-6',
      displayName: 'Docs writer',
      displayNameEdited: true,
      purpose: 'documentation',
      purposeCustom: '',
      description: 'Writes docs',
      contextWindow: '200000',
      maxOutputTokens: '8192',
    })
    const custom = modelFormFromEntry(
      entry({purpose: 'custom', purposeCustom: 'Security', displayName: 'claude-sonnet-4-6'}),
    )
    expect(custom).toMatchObject({purpose: 'custom', purposeCustom: 'Security', displayNameEdited: false})
  })

  it('pre-fills from discovery, and quick-adds with the defaults', () => {
    const item: AzureDiscoveryItem = {
      name: 'o4-mini',
      kind: 'chat',
      model: 'o4-mini',
      contextWindow: 200000,
      alreadyAdded: false,
    }
    expect(modelFormFromDiscovery(item, 'abc0123456')).toEqual({
      ...emptyModelForm('abc0123456'),
      deployment: 'o4-mini',
      displayName: 'o4-mini',
    })
    // No display name: the server gives it the name.
    expect(discoveryQuickAddInput({...item, kind: 'agent', name: 'helper'}, 'abc0123456')).toEqual({
      connectionId: 'abc0123456',
      kind: 'agent',
      deployment: 'helper',
      purpose: 'general',
    })
  })

  // U9: agent names run to 128 characters, display names to 64. The quick
  // add used to send the name as the display name, which the server refused
  // (INVALID_DISPLAY_NAME) for any agent named with 65 or more.
  it('quick-adds and pre-fills an agent whose name is longer than a display name may be', () => {
    const name = 'Review agent ' + 'x'.repeat(87)
    expect([...name]).toHaveLength(100)
    const item: AzureDiscoveryItem = {name, kind: 'agent', model: 'gpt-4o', alreadyAdded: false}

    const body = discoveryQuickAddInput(item, 'abc0123456')
    expect(body).toEqual({connectionId: 'abc0123456', kind: 'agent', deployment: name, purpose: 'general'})
    expect(body).not.toHaveProperty('displayName')

    // "Add with details…" starts on the server's default: the first 64.
    const details = modelFormFromDiscovery(item, 'abc0123456')
    expect(details.displayName).toBe(name.slice(0, 64))
    expect(details.displayNameEdited).toBe(false)
    expect(validateModelForm(details, {supportsAgents: true}).errors).toEqual({})
    expect(toModelInput(details).displayName).toHaveLength(64)
  })

  it('lets the display name follow the deployment until it is edited', () => {
    const typed = withDeployment(emptyModelForm('c'), 'gpt-4.1')
    expect(typed).toMatchObject({deployment: 'gpt-4.1', displayName: 'gpt-4.1'})
    const renamed = withDisplayName(typed, 'Everyday')
    expect(withDeployment(renamed, 'gpt-4o')).toMatchObject({deployment: 'gpt-4o', displayName: 'Everyday'})
  })

  it('follows a long agent name with its first 64 characters — counted in code points', () => {
    const name = '🤖'.repeat(70)
    const typed = withDeployment({...emptyModelForm('c'), kind: 'agent'}, name)
    expect(typed.deployment).toBe(name)
    expect([...typed.displayName]).toHaveLength(64)
    expect(validateModelForm(typed, {supportsAgents: true}).errors).toEqual({})

    // Saved like that by the server, it keeps following on edit…
    const saved = modelFormFromEntry(entry({kind: 'agent', deployment: name, displayName: typed.displayName}))!
    expect(saved.displayNameEdited).toBe(false)
    // …and a display name cleared on edit goes back to that default.
    expect(toModelUpdateInput({...saved, displayName: ' '}).displayName).toBe(typed.displayName)
  })

  it('has no edit form for a kind a newer server added: it could not send that kind back', () => {
    expect(modelFormFromEntry(entry({kind: unknownValue('realtime')}))).toBeNull()
  })

  it('suggests a kind from the name', () => {
    expect(inferKind('text-embedding-3-large')).toBe('embedding')
    expect(inferKind('Claude-Sonnet-4-6')).toBe('claude')
    expect(inferKind('gpt-4.1')).toBeNull()
  })

  it('is dirty only when a value changed', () => {
    const base = form()
    expect(modelFormsDiffer(base, {...base, displayNameEdited: true})).toBe(false)
    expect(modelFormsDiffer(base, {...base, description: 'x'})).toBe(true)
  })
})

describe('validateModelForm', () => {
  it('passes a complete form', () => {
    expect(validateModelForm(form())).toEqual({errors: {}, firstInvalid: null})
  })

  it('reports required fields in tab order', () => {
    const v = validateModelForm(emptyModelForm())
    expect(v.errors).toEqual({connectionId: 'required', deployment: 'required'})
    expect(v.firstInvalid).toBe('connectionId')
  })

  it('checks deployment and agent names', () => {
    expect(validateModelForm(form({deployment: 'my deployment'})).errors).toEqual({deployment: 'invalidName'})
    expect(validateModelForm(form({deployment: 'x'.repeat(65)})).errors).toEqual({deployment: 'invalidName'})
    expect(validateModelForm(form({kind: 'agent', deployment: 'Review bot (v2)'})).errors).toEqual({})
    expect(validateModelForm(form({kind: 'agent', deployment: 'a'.repeat(129)})).errors).toEqual({
      deployment: 'invalidName',
    })
    expect(validateModelForm(form({kind: 'agent', deployment: 'tab\there'})).errors).toEqual({
      deployment: 'invalidName',
    })
  })

  it('refuses an agent where the resource cannot run agents', () => {
    expect(validateModelForm(form({kind: 'agent', deployment: 'helper'}), {supportsAgents: false}).errors).toEqual({
      kind: 'kindNeedsProject',
    })
    expect(validateModelForm(form({kind: 'agent', deployment: 'helper'}), {supportsAgents: true}).errors).toEqual({})
  })

  // The server refuses KIND_REQUIRES_PROJECT only for a change of kind or
  // resource: an agent whose resource lost Entra ID stays editable in place.
  it('lets an unsupported agent be edited where it is, but not moved there or made one', () => {
    const agent = {kind: 'agent' as const, connectionId: 'abc0123456'}
    const edit = form({kind: 'agent', deployment: 'helper', description: 'Now with a description'})
    expect(validateModelForm(edit, {supportsAgents: false, existing: agent}).errors).toEqual({})
    // Moved to another resource that can't run it either.
    expect(
      validateModelForm({...edit, connectionId: 'other00000'}, {supportsAgents: false, existing: agent}).errors,
    ).toEqual({kind: 'kindNeedsProject'})
    // A chat model turned into an agent there.
    expect(
      validateModelForm(edit, {supportsAgents: false, existing: {kind: 'chat', connectionId: 'abc0123456'}}).errors,
    ).toEqual({kind: 'kindNeedsProject'})
  })

  it('caps the display name and the description, in characters', () => {
    expect(validateModelForm(form({displayName: '😀'.repeat(64), description: 'd'.repeat(200)})).errors).toEqual({})
    const long = validateModelForm(form({displayName: 'x'.repeat(65), description: 'd'.repeat(201)}))
    expect(long.errors).toEqual({displayName: 'tooLong', description: 'tooLong'})
    expect(long.firstInvalid).toBe('displayName')
  })

  // The server stores both cleaned (NFC, whitespace runs collapsed, trimmed)
  // and counts what it stores.
  it('counts the display name and the description as the server stores them', () => {
    const spaced = 'word' + '     '.repeat(10) + 'd'.repeat(150)
    expect(cleanLength(spaced)).toBe(155)
    expect(cleanLength('  two\n\n\tlines  ')).toBe(9)
    // 'e' + a combining acute is one character once NFC-normalised.
    expect(cleanLength('cafe' + String.fromCodePoint(0x301))).toBe(4)
    expect(validateModelForm(form({description: spaced})).errors).toEqual({})
    expect(validateModelForm(form({displayName: 'x'.repeat(32) + '   '.repeat(20) + 'y'.repeat(31)})).errors).toEqual(
      {},
    )
  })

  it('checks a custom purpose', () => {
    expect(validateModelForm(form({purpose: 'custom', purposeCustom: '  '})).errors).toEqual({purpose: 'required'})
    expect(validateModelForm(form({purpose: 'custom', purposeCustom: 'x'.repeat(33)})).errors).toEqual({
      purpose: 'tooLong',
    })
    expect(validateModelForm(form({purpose: 'custom', purposeCustom: 'Security'})).errors).toEqual({})
  })

  it('checks the advanced numbers, but not on an embedding', () => {
    expect(validateModelForm(form({contextWindow: '1024', maxOutputTokens: '1000000'})).errors).toEqual({})
    expect(validateModelForm(form({contextWindow: '1023', maxOutputTokens: '255'})).errors).toEqual({
      contextWindow: 'outOfRange',
      maxOutputTokens: 'outOfRange',
    })
    expect(validateModelForm(form({contextWindow: '2000001', maxOutputTokens: '1000001'})).errors).toEqual({
      contextWindow: 'outOfRange',
      maxOutputTokens: 'outOfRange',
    })
    expect(validateModelForm(form({contextWindow: '128k', maxOutputTokens: '1.5'})).errors).toEqual({
      contextWindow: 'notInteger',
      maxOutputTokens: 'notInteger',
    })
    expect(validateModelForm(form({kind: 'embedding', contextWindow: 'junk', maxOutputTokens: 'junk'})).errors).toEqual(
      {},
    )
  })
})

describe('request bodies', () => {
  it('creates with trimmed values, leaving blank optionals out', () => {
    expect(toModelInput(form({deployment: ' gpt-4.1 ', displayName: ' ', description: ' '}))).toEqual({
      connectionId: 'abc0123456',
      kind: 'chat',
      deployment: 'gpt-4.1',
      purpose: 'general',
    })
    expect(
      toModelInput(
        form({
          displayName: ' Everyday ',
          purpose: 'custom',
          purposeCustom: ' Security  audits ',
          description: ' Answers questions ',
          contextWindow: ' 128000 ',
          maxOutputTokens: '4096',
        }),
      ),
    ).toEqual({
      connectionId: 'abc0123456',
      kind: 'chat',
      deployment: 'gpt-4.1',
      displayName: 'Everyday',
      purpose: 'custom',
      purposeCustom: 'Security audits',
      description: 'Answers questions',
      contextWindow: 128000,
      maxOutputTokens: 4096,
    })
  })

  it('stores custom text naming a preset as the preset, and drops an invalid purpose', () => {
    expect(toModelInput(form({purpose: 'custom', purposeCustom: 'testing'}))).toMatchObject({purpose: 'testing'})
    expect(
      toModelInput(form({purpose: 'custom', purposeCustom: 'Pruebas'}), p => (p === 'testing' ? 'Pruebas' : '')),
    ).toMatchObject({
      purpose: 'testing',
    })
    expect(toModelInput(form({purpose: 'custom', purposeCustom: ''}))).not.toHaveProperty('purpose')
  })

  it('never sends the advanced numbers for an embedding', () => {
    expect(toModelInput(form({kind: 'embedding', contextWindow: '8192', maxOutputTokens: '512'}))).toEqual({
      connectionId: 'abc0123456',
      kind: 'embedding',
      deployment: 'gpt-4.1',
      displayName: 'gpt-4.1',
      purpose: 'general',
    })
  })

  it('updates every editable field, clearing the blank ones', () => {
    expect(toModelUpdateInput(form({displayName: '', description: '  '}))).toEqual({
      connectionId: 'abc0123456',
      kind: 'chat',
      deployment: 'gpt-4.1',
      displayName: 'gpt-4.1',
      purpose: 'general',
      description: '',
      contextWindow: null,
      maxOutputTokens: null,
    })
    expect(
      toModelUpdateInput(
        form({purpose: 'custom', purposeCustom: 'Security', contextWindow: '32768', maxOutputTokens: '2048'}),
      ),
    ).toMatchObject({purpose: 'custom', purposeCustom: 'Security', contextWindow: 32768, maxOutputTokens: 2048})
    expect(toModelUpdateInput(form({kind: 'embedding', contextWindow: '8192'}))).toMatchObject({
      contextWindow: null,
      maxOutputTokens: null,
    })
  })

  // U2: the edit is a compare-and-swap on the model as loaded.
  it("carries the edited model's updatedAt, and nothing when there is none", () => {
    expect(toModelUpdateInput(form(), undefined, '2026-09-30T10:00:00.123456Z')).toMatchObject({
      updatedAt: '2026-09-30T10:00:00.123456Z',
    })
    expect(toModelUpdateInput(form())).not.toHaveProperty('updatedAt')
  })
})

describe('modelErrorTarget', () => {
  const err = (code: string, status = 400) => ({code, status, message: 'x'})

  it('maps each server code to its field', () => {
    expect(modelErrorTarget(err('INVALID_DEPLOYMENT'))).toBe('deployment')
    expect(modelErrorTarget(err('DUPLICATE_MODEL', 409))).toBe('deployment')
    expect(modelErrorTarget(err('INVALID_KIND'))).toBe('kind')
    expect(modelErrorTarget(err('KIND_REQUIRES_PROJECT'))).toBe('kind')
    expect(modelErrorTarget(err('INVALID_DISPLAY_NAME'))).toBe('displayName')
    expect(modelErrorTarget(err('INVALID_PURPOSE'))).toBe('purpose')
    expect(modelErrorTarget(err('INVALID_DESCRIPTION'))).toBe('description')
    expect(modelErrorTarget(err('INVALID_CONTEXT_WINDOW'))).toBe('contextWindow')
    expect(modelErrorTarget(err('INVALID_MAX_OUTPUT'))).toBe('maxOutputTokens')
    expect(modelErrorTarget(err('CONNECTION_NOT_FOUND', 404))).toBe('connectionId')
  })

  it('sends the re-index, read-only and everything else to their places', () => {
    expect(modelErrorTarget(err('REINDEX_REQUIRED', 409))).toBe('reindex')
    expect(modelErrorTarget(err('FORBIDDEN', 403))).toBe('forbidden')
    expect(modelErrorTarget(err('TOO_MANY_MODELS', 409))).toBe('form')
    expect(modelErrorTarget(err('CONFLICT', 409))).toBe('form')
    expect(modelErrorTarget({status: 500})).toBe('form')
    expect(modelErrorTarget(undefined)).toBe('form')
  })
})
