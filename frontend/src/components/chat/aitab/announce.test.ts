import {describe, it, expect} from 'vitest'
import {answerAnnouncement, ANSWER_ANNOUNCE_MAX} from './announce'

describe('answerAnnouncement', () => {
  it('reads markdown as plain text', () => {
    expect(
      answerAnnouncement('## Summary\n\nThe **flow** calls `Invoke Url` [twice](#b1).\n\n```\ncode\n```\nDone.'),
    ).toBe('Summary The flow calls Invoke Url twice. Done.')
  })

  it('caps a long answer at a word boundary with an ellipsis', () => {
    const out = answerAnnouncement('word '.repeat(200))
    expect(out.length).toBeLessThanOrEqual(ANSWER_ANNOUNCE_MAX + 1)
    expect(out.endsWith('word…')).toBe(true)
  })

  it('keeps a short answer whole', () => {
    expect(answerAnnouncement('Yes.')).toBe('Yes.')
  })
})
