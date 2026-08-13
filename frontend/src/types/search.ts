// Search request/result shapes for the sidebar search and global search overlay.

import type {BlockType} from './flow'

export interface Highlight {
  start: number
  end: number
}

export interface SearchResult {
  blockId: string
  subflowId: string
  matchedField: string
  matchedText: string
  score: number
  highlights: Highlight[]
  /** Present only on cross-library search hits (the flow each result came from). */
  flowId?: string
  flowName?: string
}

export interface SearchResults {
  query: SearchQuery
  results: SearchResult[]
  totalCount: number
  durationMs: number
}

export interface SearchQuery {
  text: string
  blockTypes?: BlockType[]
  fuzzy: boolean
  maxResults: number
}
