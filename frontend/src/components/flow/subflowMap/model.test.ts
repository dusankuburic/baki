import {describe, it, expect} from 'vitest'
import {determineSubflowRole, computeGraphDepths, enrichGraphData} from './model'
import type {GraphData, Finding} from '@/types'

describe('subflowMap model', () => {
  it('determines roles accurately based on in/out degrees and Main name', () => {
    expect(determineSubflowRole(0, 0, false)).toBe('isolated')
    expect(determineSubflowRole(0, 3, false)).toBe('entry')
    expect(determineSubflowRole(1, 2, true)).toBe('entry')
    expect(determineSubflowRole(2, 0, false)).toBe('leaf')
    expect(determineSubflowRole(2, 2, false)).toBe('coordinator')
    expect(determineSubflowRole(1, 1, false)).toBe('intermediate')
  })

  it('computes BFS depths starting from entry nodes', () => {
    const nodes = [
      {id: 'main', label: 'Main', type: 'subflow', blockCount: 5, errorCount: 0, warnCount: 0},
      {id: 'worker', label: 'Worker', type: 'subflow', blockCount: 10, errorCount: 0, warnCount: 0},
      {id: 'helper', label: 'Helper', type: 'subflow', blockCount: 3, errorCount: 0, warnCount: 0},
    ]
    const edges = [
      {source: 'main', target: 'worker'},
      {source: 'worker', target: 'helper'},
    ]
    const inDegrees = new Map([
      ['main', 0],
      ['worker', 1],
      ['helper', 1],
    ])

    const depths = computeGraphDepths(nodes, edges, inDegrees)
    expect(depths.get('main')).toBe(0)
    expect(depths.get('worker')).toBe(1)
    expect(depths.get('helper')).toBe(2)
  })

  it('enriches graph data with finding counts and edge call counts', () => {
    const graphData: GraphData = {
      nodes: [
        {id: 'sf-main', label: 'Main', type: 'subflow', blockCount: 4, errorCount: 0, warnCount: 0},
        {id: 'sf-worker', label: 'Worker', type: 'subflow', blockCount: 8, errorCount: 0, warnCount: 0},
      ],
      edges: [
        {source: 'sf-main', target: 'sf-worker', callCount: 4, disabledCount: 1},
      ],
    }

    const findings: Finding[] = [
      {id: 'f1', ruleId: 'r1', subflowId: 'sf-worker', severity: 'error', title: 'Err', description: '', blockId: 'b1'},
      {id: 'f2', ruleId: 'r2', subflowId: 'sf-worker', severity: 'warning', title: 'Warn', description: '', blockId: 'b2'},
    ]

    const enriched = enrichGraphData(graphData, findings)
    const workerNode = enriched.nodes.find(n => n.id === 'sf-worker')!
    expect(workerNode.errorCount).toBe(1)
    expect(workerNode.warnCount).toBe(1)
    expect(workerNode.role).toBe('leaf')

    const mainNode = enriched.nodes.find(n => n.id === 'sf-main')!
    expect(mainNode.role).toBe('entry')

    const edge = enriched.edges[0]!
    expect(edge.callCount).toBe(4)
    expect(edge.disabledCount).toBe(1)
    expect(edge.enabledCount).toBe(3)
    expect(edge.isFullyDisabled).toBe(false)
    expect(edge.weight).toBe(3)
  })

  it('marks edges as fully disabled when enabledCount is 0 and disabledCount > 0', () => {
    const graphData: GraphData = {
      nodes: [
        {id: 'a', label: 'A', type: 'subflow', blockCount: 1, errorCount: 0, warnCount: 0},
        {id: 'b', label: 'B', type: 'subflow', blockCount: 1, errorCount: 0, warnCount: 0},
      ],
      edges: [
        {source: 'a', target: 'b', callCount: 2, disabledCount: 2},
      ],
    }

    const enriched = enrichGraphData(graphData)
    const edge = enriched.edges[0]!
    expect(edge.enabledCount).toBe(0)
    expect(edge.isFullyDisabled).toBe(true)
  })
})
