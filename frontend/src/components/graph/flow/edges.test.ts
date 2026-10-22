import {describe, it, expect} from 'vitest'
import {Position} from '@xyflow/react'
import {arrowMarker, handleSides, roundedPolylinePath} from './edges'

describe('roundedPolylinePath', () => {
  it('draws a straight line through two points', () => {
    expect(roundedPolylinePath([{x: 0, y: 0}, {x: 100, y: 0}])).toBe('M 0,0 L 100,0')
  })

  it('rounds a corner instead of cutting it square', () => {
    const path = roundedPolylinePath([{x: 0, y: 0}, {x: 100, y: 0}, {x: 100, y: 100}], 10)
    // Up to the corner, a quadratic through it, then on to the end.
    expect(path).toBe('M 0,0 L 90,0 Q 100,0 100,10 L 100,100')
  })

  it('never rounds further than half the shortest leg', () => {
    const path = roundedPolylinePath([{x: 0, y: 0}, {x: 10, y: 0}, {x: 10, y: 10}], 14)
    expect(path).toBe('M 0,0 L 5,0 Q 10,0 10,5 L 10,10')
  })

  it('returns nothing for a single point', () => {
    expect(roundedPolylinePath([{x: 1, y: 1}])).toBe('')
  })
})

describe('handleSides', () => {
  it('puts the ends where the layout flows', () => {
    expect(handleSides('LR')).toEqual({sourcePosition: Position.Right, targetPosition: Position.Left})
    expect(handleSides('TB')).toEqual({sourcePosition: Position.Bottom, targetPosition: Position.Top})
  })
})

describe('arrowMarker', () => {
  it('carries the colour, so React Flow makes one marker def per edge colour', () => {
    expect(arrowMarker('#abcdef')).toMatchObject({color: '#abcdef', width: 16, height: 16})
    expect(arrowMarker('#abcdef', 0.5)).toMatchObject({width: 8, height: 8})
  })
})
