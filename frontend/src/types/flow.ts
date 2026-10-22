
export type BlockType =
  | 'ACTION'
  | 'LOOP'
  | 'CONDITION'
  | 'SUBFLOW'
  | 'ERROR_HANDLER'
  | 'COMMENT'
  | 'VARIABLE'
  | 'WAIT'
  | 'BLOCK'
  | 'SWITCH'
  | 'ELSE'
  | 'CASE'
  | 'DEFAULT'
  | 'END'
  | 'UNKNOWN'

export interface BlockToken {
  type: 'text' | 'variable' | 'subflow' | 'label' | 'string'
  value: string
  target?: string
}

export interface Block {
  id: string
  name: string
  type: BlockType
  rawType: string
  indent: number
  lineNumber: number
  children: Block[]
  properties: Record<string, string>
  variables: string[]
  tokens?: BlockToken[]
  parentId?: string
  subflowId: string
}

export interface Subflow {
  id: string
  name: string
  sourceFile?: string
  blocks: Block[]
  variables: VariableDecl[]
}

export interface VariableDecl {
  name: string
  type: string
  initialValue?: string
  scope: 'flow' | 'subflow'
}

export interface VariableHistory {
  name: string
  events: VariableEvent[]
}

export interface VariableEvent {
  type: 'init' | 'mutate' | 'read'
  blockId: string
  line: number
  subflowId: string
}

export interface FlowDocument {
  id: string
  name: string
  filePath: string
  subflows: Subflow[]
  metadata: FlowMetadata
  files?: FlowFileInfo[]
  isFolder?: boolean
  parseErrors?: ParseError[]
  orgId?: string
}

export interface FlowMetadata {
  blockCount: number
  subflowCount: number
  maxDepth: number
  parsedAt: string
  fileSize: number
  rawLineCount: number
}

export interface ParseError {
  line: number
  column?: number
  message: string
  severity: 'warning' | 'error'
  snippet: string
}

export interface RecentFile {
  path: string
  name: string
  size: number
  lastOpen: string
  isFolder?: boolean
}

export interface FlowFileInfo {
  path: string
  name: string
  size: number
}

export interface GraphNode {
  id: string
  label: string
  type: string
  blockCount: number
  errorCount: number
  warnCount: number
}

export interface GraphEdge {
  source: string
  target: string
  callCount?: number
  disabledCount?: number
}

export interface GraphData {
  nodes: GraphNode[]
  edges: GraphEdge[]
}
