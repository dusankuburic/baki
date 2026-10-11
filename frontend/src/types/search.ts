
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
