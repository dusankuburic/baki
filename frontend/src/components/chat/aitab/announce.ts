import type {ChatMessage} from '@/types'

/** The longest answer prefix the status region reads out on commit (R-frontend-5). */
export const ANSWER_ANNOUNCE_MAX = 280

/**
 * The plain-text prefix of an answer for a screen reader: markdown markup and code blocks dropped, whitespace
 * collapsed, cut at a word boundary to ANSWER_ANNOUNCE_MAX characters with an ellipsis when it was longer.
 */
export function answerAnnouncement(content: string): string {
  const plain = content
    .replace(/```[\s\S]*?(```|$)/g, ' ')
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/^\s{0,3}(#{1,6}|>|[-*+]|\d+\.)\s+/gm, '')
    .replace(/[*_`~]+/g, '')
    .replace(/^\s*-{3,}\s*$/gm, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  if (plain.length <= ANSWER_ANNOUNCE_MAX) return plain
  const cut = plain.slice(0, ANSWER_ANNOUNCE_MAX)
  const space = cut.lastIndexOf(' ')
  return `${(space > ANSWER_ANNOUNCE_MAX / 2 ? cut.slice(0, space) : cut).trimEnd()}…`
}

/**
 * The first assistant message after the message `afterId` (from the start when it is undefined): a turn's answer, or
 * its error bubble. Undefined when there is none yet, or when `afterId` is no longer in the list (it was replaced).
 */
export function answerAfter(messages: readonly ChatMessage[], afterId: string | undefined): ChatMessage | undefined {
  let from = 0
  if (afterId !== undefined) {
    const at = messages.findIndex(m => m.id === afterId)
    if (at < 0) return undefined
    from = at + 1
  }
  for (let i = from; i < messages.length; i++) {
    if (messages[i]!.role === 'assistant') return messages[i]
  }
  return undefined
}
