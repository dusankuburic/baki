import {describe, it, expect} from 'vitest'
import {parseChatEvent, chatEventStreamId, parseResumeEvents} from './chatEvent'

describe('parseChatEvent', () => {
  it('parses a chunk envelope with content', () => {
    expect(parseChatEvent({streamId: 's1', type: 'chunk', data: {content: 'hel'}})).toEqual({
      kind: 'chunk',
      content: 'hel',
    })
  })

  it('defaults a missing chunk content to empty string (RAF tail tolerates it)', () => {
    expect(parseChatEvent({streamId: 's1', type: 'chunk', data: {}})).toEqual({kind: 'chunk', content: ''})
  })

  it('parses a done envelope with token counts and chunk total', () => {
    expect(parseChatEvent({streamId: 's1', type: 'done', data: {tokensOut: 42, tokensIn: 10, chunks: 7}})).toEqual({
      kind: 'done',
      tokensOut: 42,
      tokensIn: 10,
      chunks: 7,
    })
  })

  it('tolerates missing counts on done', () => {
    expect(parseChatEvent({streamId: 's1', type: 'done'})).toEqual({
      kind: 'done',
      tokensOut: 0,
      tokensIn: 0,
      chunks: undefined,
    })
  })

  it('parses an error envelope, defaulting the message', () => {
    expect(parseChatEvent({streamId: 's1', type: 'error', data: {message: 'boom'}})).toEqual({
      kind: 'error',
      message: 'boom',
    })
    expect(parseChatEvent({streamId: 's1', type: 'error'})).toEqual({kind: 'error', message: 'Unknown error'})
  })

  it('parses a tool envelope preferring label over name', () => {
    expect(parseChatEvent({streamId: 's1', type: 'tool', data: {label: 'Reading file', name: 'read'}})).toEqual({
      kind: 'tool',
      label: 'Reading file',
    })
    expect(parseChatEvent({streamId: 's1', type: 'tool', data: {name: 'read'}})).toEqual({
      kind: 'tool',
      label: 'read',
    })
    expect(parseChatEvent({streamId: 's1', type: 'tool'})).toEqual({kind: 'tool', label: 'Using tool'})
  })

  it('parses a fix-proposal envelope (single fix normalizes to items[1])', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_proposal',
        data: {
          proposalId: 'p1',
          ruleId: 'unhandled-error',
          fixType: 'wrap-error-handler',
          blockLabel: 'Call API',
          line: 3,
          summary: 'wrap',
        },
      }),
    ).toEqual({
      kind: 'fix-proposal',
      proposalId: 'p1',
      items: [
        {ruleId: 'unhandled-error', fixType: 'wrap-error-handler', blockLabel: 'Call API', line: 3, summary: 'wrap'},
      ],
    })
    // Batch: items[] pass through with per-item defaults; missing optional
    // fields degrade per item.
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_proposal',
        data: {proposalId: 'p2', batch: true, count: 2, items: [{ruleId: 'r1'}, {ruleId: 'r2', fixType: 'f'}]},
      }),
    ).toEqual({
      kind: 'fix-proposal',
      proposalId: 'p2',
      items: [
        {ruleId: 'r1', fixType: '', blockLabel: '', line: 0, summary: ''},
        {ruleId: 'r2', fixType: 'f', blockLabel: '', line: 0, summary: ''},
      ],
    })
    expect(parseChatEvent({streamId: 's1', type: 'fix_proposal', data: {}})).toBeNull()
  })

  it('parses batch fix-decision items alongside the overall status', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {
          proposalId: 'p2',
          status: 'applied-unresolved',
          items: [
            {ruleId: 'r1', status: 'applied'},
            {ruleId: 'r2', status: 'error', message: 'boom'},
          ],
        },
      }),
    ).toEqual({
      kind: 'fix-decision',
      proposalId: 'p2',
      status: 'applied-unresolved',
      message: undefined,
      items: [
        {ruleId: 'r1', status: 'applied', message: undefined},
        {ruleId: 'r2', status: 'error', message: 'boom'},
      ],
    })
    // Items missing ruleId/status drop; no items → undefined.
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {proposalId: 'p', status: 'applied', items: [{status: 'applied'}, 'junk']},
      }),
    ).toEqual({kind: 'fix-decision', proposalId: 'p', status: 'applied', message: undefined, items: undefined})
  })

  it('parseResumeEvents converts a resume journal into replayable events', () => {
    expect(
      parseResumeEvents([
        {
          type: 'tool_result',
          data: {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 3, summary: '2 matches'},
        },
        {
          type: 'fix_proposal',
          data: {proposalId: 'p1', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'},
        },
        {type: 'fix_decision', data: {proposalId: 'p1', status: 'applied'}},
        {type: 'chunk', data: {content: 'not replayable'}},
        'junk',
      ]),
    ).toEqual([
      {
        kind: 'tool-result',
        name: 'search_flow',
        label: 'Searching flow',
        ok: true,
        durationMs: 3,
        summary: '2 matches',
      },
      {
        kind: 'fix-proposal',
        proposalId: 'p1',
        items: [{ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
      },
      {kind: 'fix-decision', proposalId: 'p1', status: 'applied', message: undefined, items: undefined},
      // Chunks parse (a well-formed event) though the backend journal never
      // records them — the replay consumer ignores non-agentic kinds.
      {kind: 'chunk', content: 'not replayable'},
    ])
    expect(parseResumeEvents(undefined)).toEqual([])
  })

  it('parses a fix-decision envelope, requiring proposalId + status', () => {
    expect(parseChatEvent({streamId: 's1', type: 'fix_decision', data: {proposalId: 'p1', status: 'applied'}})).toEqual(
      {kind: 'fix-decision', proposalId: 'p1', status: 'applied', message: undefined},
    )
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {proposalId: 'p1', status: 'error', message: 'boom'},
      }),
    ).toEqual({kind: 'fix-decision', proposalId: 'p1', status: 'error', message: 'boom'})
    expect(parseChatEvent({streamId: 's1', type: 'fix_decision', data: {proposalId: 'p1'}})).toBeNull()
    expect(parseChatEvent({streamId: 's1', type: 'fix_decision', data: {status: 'applied'}})).toBeNull()
  })

  it('parses a tool_result envelope, requiring name and defaulting the rest', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'tool_result',
        data: {name: 'search_flow', label: 'Searching flow', ok: true, durationMs: 12, summary: '3 matches'},
      }),
    ).toEqual({
      kind: 'tool-result',
      name: 'search_flow',
      label: 'Searching flow',
      ok: true,
      durationMs: 12,
      summary: '3 matches',
    })
    // Missing fields degrade: label falls back to name, ok is only ever true
    // when the wire said so, duration defaults 0, summary ''.
    expect(parseChatEvent({streamId: 's1', type: 'tool_result', data: {name: 'read_doc'}})).toEqual({
      kind: 'tool-result',
      name: 'read_doc',
      label: 'read_doc',
      ok: false,
      durationMs: 0,
      summary: '',
    })
    expect(parseChatEvent({streamId: 's1', type: 'tool_result', data: {}})).toBeNull()
  })

  it('drops malformed payloads instead of casting them into the store', () => {
    // A proxy error page / different event's shape / truncated JSON.
    expect(parseChatEvent(null)).toBeNull()
    expect(parseChatEvent('<html>err</html>')).toBeNull()
    expect(parseChatEvent([])).toBeNull()
    expect(parseChatEvent({noStreamId: true, type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: '', type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: 's1', type: 'surprise'})).toBeNull()
    expect(parseChatEvent({streamId: 's1'})).toBeNull()
    // Non-finite numbers must not leak into token accounting.
    expect(parseChatEvent({streamId: 's1', type: 'done', data: {tokensOut: 'lots'}})).toEqual({
      kind: 'done',
      tokensOut: 0,
      tokensIn: 0,
      chunks: undefined,
    })
  })
})

describe('chatEventStreamId', () => {
  it('extracts the addressing streamId', () => {
    expect(chatEventStreamId({streamId: 's1', type: 'chunk'})).toBe('s1')
  })

  it('returns null for malformed envelopes', () => {
    expect(chatEventStreamId(undefined)).toBeNull()
    expect(chatEventStreamId({streamId: 42})).toBeNull()
    expect(chatEventStreamId('s1')).toBeNull()
  })
})
