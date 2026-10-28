import {describe, it, expect} from 'vitest'
import {parseChatEvent, parseResumeEvents} from './chatEvent'
import {chatEventGolden, chatEventGoldenNames, type ChatEventEnvelope} from '@/testing/chatEventGoldens'

/**
 * The chat event contract (R-testing-3): every envelope the backend emits, as its golden records it
 * (internal/service/testdata/chat_events, refreshed with -update-chat-events), parses to an event that keeps every
 * field the backend sends — a renamed or dropped field fails here instead of reaching a user as a blank card.
 */

/** Fields the client deliberately does not keep, by event type, each with its reason. */
const IGNORED: Record<string, Record<string, string>> = {
  tool: {name: 'the status line shows the label; the tool name arrives again on tool_result'},
  fix_proposal: {
    batch: 'the card is a batch when it has more than one item',
    count: 'the card counts its items',
  },
}

type Leaf = {path: string; key: string; value: unknown}

/** Every leaf of v with its full path, array elements by index (items[1].blockLabel). */
function leaves(v: unknown, path = ''): Leaf[] {
  if (Array.isArray(v)) return v.flatMap((x, i) => leaves(x, `${path}[${i}]`))
  if (v && typeof v === 'object') {
    return Object.entries(v as Record<string, unknown>).flatMap(([k, x]) => {
      const p = path ? `${path}.${k}` : k
      return x && typeof x === 'object' ? leaves(x, p) : [{path: p, key: k, value: x}]
    })
  }
  return []
}

/** Where the parsed event keeps a backend field, by event type, where the path differs. */
const PATHS: Record<string, Record<string, string>> = {
  notice: {kind: 'notice'},
  error: {conversationVersion: 'meta.conversationVersion', conversationBase: 'meta.conversationBase'},
  // A single fix repeats its one item as flat fields for older clients; the parser reads the item.
  fix_proposal: {
    ruleId: 'items[0].ruleId',
    fixType: 'items[0].fixType',
    blockId: 'items[0].blockId',
    blockLabel: 'items[0].blockLabel',
    line: 'items[0].line',
    summary: 'items[0].summary',
  },
}

describe('chat event contract', () => {
  const names = chatEventGoldenNames()

  it('has a golden for every event type the client handles', () => {
    for (const want of ['chunk', 'done', 'error', 'heartbeat', 'tool', 'tool_result', 'notice_history_truncated']) {
      expect(names).toContain(want)
    }
    expect(names.some(n => n.startsWith('fix_proposal'))).toBe(true)
    expect(names.some(n => n.startsWith('fix_decision'))).toBe(true)
    expect(names).toContain('resume')
  })

  for (const name of names.filter(n => n !== 'resume')) {
    it(`${name}: parses and keeps every field the backend sends`, () => {
      const env = chatEventGolden<ChatEventEnvelope>(name)
      const parsed = parseChatEvent(env)
      expect(parsed, `${name} parsed to null`).not.toBeNull()
      const kept = new Map(leaves(parsed).map(l => [l.path, l.value]))
      const ignored = IGNORED[env.type] ?? {}
      const paths = PATHS[env.type] ?? {}
      for (const leaf of leaves(env.data)) {
        if (leaf.path in ignored) continue
        const at = paths[leaf.path] ?? leaf.path
        expect(
          kept.get(at),
          `${name}: the backend's ${leaf.path} = ${JSON.stringify(leaf.value)} is not at ${at} after parsing`,
        ).toBe(leaf.value)
      }
    })
  }

  it('resume: every journal event of the body parses', () => {
    const body = chatEventGolden<{events: unknown[]; text: string; done: boolean}>('resume')
    expect(body.events.length).toBeGreaterThan(0)
    expect(parseResumeEvents(body.events)).toHaveLength(body.events.length)
    expect(typeof body.text).toBe('string')
    expect(typeof body.done).toBe('boolean')
  })
})
