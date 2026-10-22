import {describe, it, expect} from 'vitest'
import {NODE_CARD_HEIGHT, NODE_CARD_WIDTH, layoutSubflowMap, mapEdgeId} from './layout'
import {enrichGraphData} from './model'
import type {GraphData} from '@/types'

const node = (id: string, label: string) => ({
  id,
  label,
  type: 'subflow',
  blockCount: 1,
  errorCount: 0,
  warnCount: 0,
})

const chain: GraphData = {
  nodes: [node('a', 'Main'), node('b', 'Middle'), node('c', 'End')],
  edges: [
    {source: 'a', target: 'b'},
    {source: 'b', target: 'c'},
  ],
}

describe('subflowMap layout', () => {
  it('lays a hierarchy out left to right and returns card corners', () => {
    const result = layoutSubflowMap(enrichGraphData(chain), 'hierarchy', 'LR', 'corridor')

    expect(result.positions.size).toBe(3)
    const a = result.positions.get('a')!
    const b = result.positions.get('b')!
    const c = result.positions.get('c')!
    expect(a.x).toBeLessThan(b.x)
    expect(b.x).toBeLessThan(c.x)
    // Corners, not centres: a card's left edge is a full card-width clear of the next rank.
    expect(b.x - a.x).toBeGreaterThanOrEqual(NODE_CARD_WIDTH)
  })

  it('lays a hierarchy out top to bottom', () => {
    const result = layoutSubflowMap(enrichGraphData(chain), 'hierarchy', 'TB', 'corridor')

    const a = result.positions.get('a')!
    const b = result.positions.get('b')!
    expect(a.y).toBeLessThan(b.y)
    expect(b.y - a.y).toBeGreaterThanOrEqual(NODE_CARD_HEIGHT)
  })

  it('routes a rank-skipping edge through bend points under corridor routing, and not otherwise', () => {
    const skipping: GraphData = {
      nodes: chain.nodes,
      edges: [...chain.edges, {source: 'a', target: 'c'}],
    }
    const enriched = enrichGraphData(skipping)

    const corridor = layoutSubflowMap(enriched, 'hierarchy', 'LR', 'corridor')
    const points = corridor.points.get(mapEdgeId('a', 'c'))
    expect(points?.length ?? 0).toBeGreaterThan(2)

    const straight = layoutSubflowMap(enriched, 'hierarchy', 'LR', 'straight')
    expect(straight.points.size).toBe(0)
  })

  it('spreads a radial layout around the entry node', () => {
    const star: GraphData = {
      nodes: [node('a', 'Main'), node('b', 'One'), node('c', 'Two'), node('d', 'Three')],
      edges: [
        {source: 'a', target: 'b'},
        {source: 'a', target: 'c'},
        {source: 'a', target: 'd'},
      ],
    }

    const result = layoutSubflowMap(enrichGraphData(star), 'radial', 'LR', 'straight')

    expect(result.positions.size).toBe(4)
    expect(result.points.size).toBe(0)
    const centre = result.positions.get('a')!
    const ring = ['b', 'c', 'd'].map(id => result.positions.get(id)!)
    const radii = ring.map(p => Math.hypot(p.x - centre.x, p.y - centre.y))
    for (const radius of radii) expect(radius).toBeGreaterThan(NODE_CARD_HEIGHT)
    // One ring: every node the same distance out.
    expect(Math.max(...radii) - Math.min(...radii)).toBeLessThan(1)
  })

  it('packs disconnected subflows into separate clusters', () => {
    const islands: GraphData = {
      nodes: [node('a', 'Main'), node('b', 'Leaf'), node('x', 'Other'), node('y', 'OtherLeaf')],
      edges: [
        {source: 'a', target: 'b'},
        {source: 'x', target: 'y'},
      ],
    }

    const result = layoutSubflowMap(enrichGraphData(islands), 'cluster', 'TB', 'straight')

    expect(result.positions.size).toBe(4)
    const a = result.positions.get('a')!
    const x = result.positions.get('x')!
    // The two components do not sit on top of each other.
    expect(Math.abs(a.x - x.x) > NODE_CARD_WIDTH || Math.abs(a.y - x.y) > NODE_CARD_HEIGHT).toBe(true)
  })

  it('is stable: the same graph lays out identically twice', () => {
    const first = layoutSubflowMap(enrichGraphData(chain), 'cluster', 'TB', 'straight')
    const second = layoutSubflowMap(enrichGraphData(chain), 'cluster', 'TB', 'straight')
    expect([...second.positions.entries()]).toEqual([...first.positions.entries()])
  })
})
