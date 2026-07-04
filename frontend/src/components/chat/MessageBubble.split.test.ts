import {describe, it, expect} from 'vitest'
import {splitStreamingContent} from './MessageBubble'

describe('splitStreamingContent', () => {
  it('keeps everything in the tail when there is no paragraph boundary', () => {
    expect(splitStreamingContent('one long unbroken paragraph')).toEqual(['', 'one long unbroken paragraph'])
  })

  it('splits at the last completed paragraph boundary', () => {
    const content = 'first para\n\nsecond para\n\nstill streaming'
    expect(splitStreamingContent(content)).toEqual(['first para\n\nsecond para\n\n', 'still streaming'])
  })

  it('puts an empty tail after a trailing boundary', () => {
    expect(splitStreamingContent('done para\n\n')).toEqual(['done para\n\n', ''])
  })

  it('refuses to split inside an unterminated code fence', () => {
    const content = 'intro\n\n```go\nfunc main() {\n\nfmt.Println("hi")'
    expect(splitStreamingContent(content)).toEqual(['', content])
  })

  it('splits after a closed code fence', () => {
    const content = 'intro\n\n```go\ncode\n```\n\ntail text'
    expect(splitStreamingContent(content)).toEqual(['intro\n\n```go\ncode\n```\n\n', 'tail text'])
  })

  it('head + tail always reassemble to the original content', () => {
    const samples = [
      '',
      'plain',
      'a\n\nb\n\nc',
      '```\nopen fence\n\nmore',
      'x\n\n```\nclosed\n```\n\ny\n\nz',
    ]
    for (const s of samples) {
      const [head, tail] = splitStreamingContent(s)
      expect(head + tail).toBe(s)
    }
  })
})
