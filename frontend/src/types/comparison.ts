// Flow-vs-flow comparison types used by the Diff view and version compare flows.

import type {Block} from './flow';

export type ChangeType = 'none' | 'added' | 'removed' | 'modified';

export interface FlowDiff {
  oldId: string;
  newId: string;
  subflows: SubflowDiff[];
}

export interface SubflowDiff {
  name: string;
  change: ChangeType;
  blocks: BlockDiff[];
}

export interface BlockDiff {
  change: ChangeType;
  old?: Block;
  new?: Block;
}

export interface FlowComparison {
  flowAId: string;
  flowBId: string;
  subflowDiff: SubflowComparison[];
  sharedBlocks: number;
  addedBlocks: number;
  removedBlocks: number;
  similarity: number;
}

export interface SubflowComparison {
  subflowA: string;
  subflowB: string;
  blockDiffs: BlockComparison[];
  similarity: number;
}

export interface BlockComparison {
  blockA?: Block;
  blockB?: Block;
  change: string;
  similarity?: number;
}
