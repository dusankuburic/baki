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

  it('carries the served model and billed rates when the backend sent them on done', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'done',
        data: {tokensOut: 4, tokensIn: 2, servedModel: 'gpt-4o-2024-08-06', inputCostPerM: 2.5, outputCostPerM: 10},
      }),
    ).toEqual({
      kind: 'done',
      tokensOut: 4,
      tokensIn: 2,
      chunks: undefined,
      servedModel: 'gpt-4o-2024-08-06',
    })
  })

  it('omits served-model fields that are absent, empty or of the wrong type', () => {
    const plain = parseChatEvent({streamId: 's1', type: 'done', data: {tokensOut: 1, tokensIn: 1}})
    expect(plain).toEqual({kind: 'done', tokensOut: 1, tokensIn: 1, chunks: undefined})
    expect(Object.keys(plain!)).toEqual(['kind', 'tokensOut', 'tokensIn', 'chunks'])

    const junk = parseChatEvent({
      streamId: 's1',
      type: 'done',
      data: {tokensOut: 1, tokensIn: 1, servedModel: '', inputCostPerM: '3'},
    })
    expect(Object.keys(junk!)).toEqual(['kind', 'tokensOut', 'tokensIn', 'chunks'])
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
      source: 'fix',
      proposalId: 'p1',
      tool: '',
      previewKind: 'fix-patch',
      items: [
        {
          id: 'unhandled-error',
          label: 'Call API',
          ruleId: 'unhandled-error',
          fixType: 'wrap-error-handler',
          blockLabel: 'Call API',
          line: 3,
          summary: 'wrap',
        },
      ],
    })
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_proposal',
        data: {proposalId: 'p2', batch: true, count: 2, items: [{ruleId: 'r1'}, {ruleId: 'r2', fixType: 'f'}]},
      }),
    ).toEqual({
      kind: 'fix-proposal',
      source: 'fix',
      proposalId: 'p2',
      tool: '',
      previewKind: 'fix-patch',
      items: [
        {id: 'r1', label: '', ruleId: 'r1', fixType: '', blockLabel: '', line: 0, summary: ''},
        {id: 'r2', label: '', ruleId: 'r2', fixType: 'f', blockLabel: '', line: 0, summary: ''},
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
      source: 'fix',
      proposalId: 'p2',
      status: 'applied-unresolved',
      message: undefined,
      items: [
        {ruleId: 'r1', status: 'applied', message: undefined},
        {ruleId: 'r2', status: 'error', message: 'boom'},
      ],
    })
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {proposalId: 'p', status: 'applied', items: [{status: 'applied'}, 'junk']},
      }),
    ).toEqual({
      kind: 'fix-decision',
      source: 'fix',
      proposalId: 'p',
      status: 'applied',
      message: undefined,
      items: undefined,
    })
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
        source: 'fix',
        proposalId: 'p1',
        tool: '',
        previewKind: 'fix-patch',
        items: [{id: 'r', label: 'b', ruleId: 'r', fixType: 'f', blockLabel: 'b', line: 1, summary: 's'}],
      },
      {
        kind: 'fix-decision',
        source: 'fix',
        proposalId: 'p1',
        status: 'applied',
        message: undefined,
        items: undefined,
      },
      {kind: 'chunk', content: 'not replayable'},
    ])
    expect(parseResumeEvents(undefined)).toEqual([])
  })

  it('parses a fix-decision envelope, requiring proposalId + status', () => {
    expect(parseChatEvent({streamId: 's1', type: 'fix_decision', data: {proposalId: 'p1', status: 'applied'}})).toEqual(
      {kind: 'fix-decision', source: 'fix', proposalId: 'p1', status: 'applied', message: undefined},
    )
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {proposalId: 'p1', status: 'error', message: 'boom'},
      }),
    ).toEqual({kind: 'fix-decision', source: 'fix', proposalId: 'p1', status: 'error', message: 'boom'})
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
    expect(parseChatEvent(null)).toBeNull()
    expect(parseChatEvent('<html>err</html>')).toBeNull()
    expect(parseChatEvent([])).toBeNull()
    expect(parseChatEvent({noStreamId: true, type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: '', type: 'chunk'})).toBeNull()
    expect(parseChatEvent({streamId: 's1', type: 'surprise'})).toBeNull()
    expect(parseChatEvent({streamId: 's1'})).toBeNull()
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

describe('parseChatEvent failure codes and keepalives', () => {
  it('carries the machine-readable code of an error event', () => {
    expect(
      parseChatEvent({streamId: 's1', type: 'error', data: {message: 'budget', code: 'AI_BUDGET_EXCEEDED'}}),
    ).toEqual({kind: 'error', message: 'budget', code: 'AI_BUDGET_EXCEEDED'})
  })

  it('omits the code when the backend sent none', () => {
    expect(parseChatEvent({streamId: 's1', type: 'error', data: {message: 'x'}})).toEqual({
      kind: 'error',
      message: 'x',
    })
  })

  it('parses a heartbeat', () => {
    expect(parseChatEvent({streamId: 's1', type: 'heartbeat', data: {}})).toEqual({kind: 'heartbeat'})
  })

  it('carries the usage of a billed error event', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'error',
        data: {
          message: 'raise Max tokens',
          code: 'PROVIDER_OUTPUT_LIMIT',
          tokensIn: 50,
          tokensOut: 4096,
          servedModel: 'gpt-5.1',
        },
      }),
    ).toEqual({
      kind: 'error',
      message: 'raise Max tokens',
      code: 'PROVIDER_OUTPUT_LIMIT',
      tokensIn: 50,
      tokensOut: 4096,
      meta: {servedModel: 'gpt-5.1'},
    })
  })

  it('keeps an unbilled error event free of usage keys', () => {
    const parsed = parseChatEvent({
      streamId: 's1',
      type: 'error',
      data: {message: 'x', code: 'PROVIDER_AUTH', tokensIn: 'junk', tokensOut: Number.NaN},
    })
    expect(parsed).toEqual({kind: 'error', message: 'x', code: 'PROVIDER_AUTH'})
  })

  it('keeps the shared flag of a shared-credential error event', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'error',
        data: {message: 'ask your admin', code: 'PROVIDER_NOT_CONFIGURED', shared: true},
      }),
    ).toEqual({kind: 'error', message: 'ask your admin', code: 'PROVIDER_NOT_CONFIGURED', shared: true})
    expect(parseChatEvent({streamId: 's1', type: 'error', data: {message: 'mine', shared: 'yes'}})).toEqual({
      kind: 'error',
      message: 'mine',
    })
  })
})

describe('parseChatEvent approvals', () => {
  it('reads tool, previewKind, requiredScope and items[] off a fix_proposal', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_proposal',
        data: {
          proposalId: 'p1',
          tool: 'apply_fix',
          previewKind: 'fix-patch',
          requiredScope: 'write',
          ruleId: 'unhandled-error',
          fixType: 'wrap-error-handler',
          blockLabel: 'Call API',
          line: 3,
          summary: 'wrap',
          items: [
            {
              ruleId: 'unhandled-error',
              fixType: 'wrap-error-handler',
              blockLabel: 'Call API',
              line: 3,
              summary: 'wrap',
            },
          ],
        },
      }),
    ).toEqual({
      kind: 'fix-proposal',
      source: 'fix',
      proposalId: 'p1',
      tool: 'apply_fix',
      previewKind: 'fix-patch',
      requiredScope: 'write',
      items: [
        {
          id: 'unhandled-error',
          label: 'Call API',
          ruleId: 'unhandled-error',
          fixType: 'wrap-error-handler',
          blockLabel: 'Call API',
          line: 3,
          summary: 'wrap',
        },
      ],
    })
  })

  it('parses approval_request/approval_decision; a fix_proposal maps to previewKind fix-patch', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'approval_request',
        data: {
          proposalId: 'a1',
          tool: 'rename_variable',
          previewKind: 'something-new',
          requiredScope: 'write',
          items: [
            {id: 'v1', label: 'Variable %A%', summary: 'to %Alpha%'},
            {
              id: 'r1',
              label: 'Call API',
              summary: 'wrap',
              meta: {ruleId: 'r1', fixType: 'f', blockLabel: 'Call API', line: 3},
            },
          ],
        },
      }),
    ).toEqual({
      kind: 'fix-proposal',
      source: 'approval',
      proposalId: 'a1',
      tool: 'rename_variable',
      previewKind: 'something-new',
      requiredScope: 'write',
      items: [
        {id: 'v1', label: 'Variable %A%', ruleId: '', fixType: '', blockLabel: '', line: 0, summary: 'to %Alpha%'},
        {id: 'r1', label: 'Call API', ruleId: 'r1', fixType: 'f', blockLabel: 'Call API', line: 3, summary: 'wrap'},
      ],
    })
    expect(parseChatEvent({streamId: 's1', type: 'fix_proposal', data: {proposalId: 'p1', ruleId: 'r'}})).toMatchObject(
      {source: 'fix', previewKind: 'fix-patch'},
    )
    expect(
      parseChatEvent({streamId: 's1', type: 'approval_request', data: {proposalId: 'a2', items: [{id: 'x'}]}}),
    ).toMatchObject({source: 'approval', previewKind: 'fix-patch', items: [{id: 'x', label: '', ruleId: ''}]})
    expect(parseChatEvent({streamId: 's1', type: 'approval_request', data: {}})).toBeNull()

    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'approval_decision',
        data: {
          proposalId: 'a1',
          status: 'cancelled',
          message: 'the server is restarting',
          items: [
            {id: 'v1', status: 'error'},
            {ruleId: 'r1', status: 'applied'},
          ],
        },
      }),
    ).toEqual({
      kind: 'fix-decision',
      source: 'approval',
      proposalId: 'a1',
      status: 'cancelled',
      message: 'the server is restarting',
      items: [
        {id: 'v1', status: 'error', message: undefined},
        {ruleId: 'r1', status: 'applied', message: undefined},
      ],
    })
    expect(parseChatEvent({streamId: 's1', type: 'approval_decision', data: {proposalId: 'a1'}})).toBeNull()
  })

  it('keys decision items by the id the server sent, and invents none from the rule', () => {
    const request = parseChatEvent({
      streamId: 's1',
      type: 'fix_proposal',
      data: {
        proposalId: 'p3',
        items: [
          {id: 'b1:unhandled-error', ruleId: 'unhandled-error', blockId: 'b1', blockLabel: 'Call A'},
          {id: 'b2:unhandled-error', ruleId: 'unhandled-error', blockId: 'b2', blockLabel: 'Call B'},
        ],
      },
    })
    expect(request).toMatchObject({
      items: [
        {id: 'b1:unhandled-error', ruleId: 'unhandled-error', label: 'Call A'},
        {id: 'b2:unhandled-error', ruleId: 'unhandled-error', label: 'Call B'},
      ],
    })
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'fix_decision',
        data: {
          proposalId: 'p3',
          status: 'applied-unresolved',
          items: [
            {id: 'b1:unhandled-error', ruleId: 'unhandled-error', status: 'applied'},
            {id: 'b2:unhandled-error', ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
          ],
        },
      }),
    ).toEqual({
      kind: 'fix-decision',
      source: 'fix',
      proposalId: 'p3',
      status: 'applied-unresolved',
      message: undefined,
      items: [
        {id: 'b1:unhandled-error', ruleId: 'unhandled-error', status: 'applied', message: undefined},
        {id: 'b2:unhandled-error', ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
      ],
    })
    const legacy = parseChatEvent({
      streamId: 's1',
      type: 'fix_decision',
      data: {
        proposalId: 'p3',
        status: 'applied-unresolved',
        items: [
          {ruleId: 'unhandled-error', status: 'applied'},
          {ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
        ],
      },
    })
    expect(legacy).toMatchObject({
      items: [
        {ruleId: 'unhandled-error', status: 'applied'},
        {ruleId: 'unhandled-error', status: 'error', message: 'block changed'},
      ],
    })
    const legacyItems = legacy?.kind === 'fix-decision' ? legacy.items! : []
    expect(legacyItems[0]).not.toHaveProperty('id')
    expect(legacyItems[1]).not.toHaveProperty('id')
  })
})

describe('conversation version on the terminal events', () => {
  it('reads the stored version and the version the turn began from off done', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'done',
        data: {tokensOut: 1, tokensIn: 1, conversationVersion: 42, conversationBase: 40},
      }),
    ).toMatchObject({kind: 'done', conversationVersion: 42, conversationBase: 40})
  })

  it('reads them off an error, which keeps them in its meta with the rates', () => {
    expect(
      parseChatEvent({
        streamId: 's1',
        type: 'error',
        data: {message: 'x', conversationVersion: 7, conversationBase: 0},
      }),
    ).toMatchObject({kind: 'error', meta: {conversationVersion: 7, conversationBase: 0}})
  })

  it('ignores a version that is not a non-negative whole number', () => {
    for (const bad of [-1, 1.5, '7', null, Number.NaN]) {
      const ev = parseChatEvent({
        streamId: 's1',
        type: 'done',
        data: {tokensOut: 1, tokensIn: 1, conversationVersion: bad, conversationBase: bad},
      })
      expect(ev).not.toHaveProperty('conversationVersion')
      expect(ev).not.toHaveProperty('conversationBase')
    }
  })
})

// The history-truncation notice (R-streaming-7): a journaled event the client parses live and on resume.
describe('parseChatEvent notice', () => {
  it('parses a history_truncated notice with its counts', () => {
    expect(
      parseChatEvent({streamId: 's1', type: 'notice', data: {kind: 'history_truncated', dropped: 3, keptFrom: 3}}),
    ).toEqual({kind: 'notice', notice: 'history_truncated', dropped: 3, keptFrom: 3})
  })

  it('drops a notice without a kind', () => {
    expect(parseChatEvent({streamId: 's1', type: 'notice', data: {}})).toBeNull()
  })

  it('replays a journaled notice on resume', () => {
    expect(parseResumeEvents([{type: 'notice', data: {kind: 'history_truncated', dropped: 2, keptFrom: 5}}])).toEqual([
      {kind: 'notice', notice: 'history_truncated', dropped: 2, keptFrom: 5},
    ])
  })
})
