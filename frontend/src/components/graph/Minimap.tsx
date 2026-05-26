import {useRef, useEffect, useCallback} from 'react'
import type cytoscape from 'cytoscape'
import type {GraphTokenColors} from './graphStyle'

interface Props {
  cy: cytoscape.Core | null
  tokens?: GraphTokenColors
  width?: number
  height?: number
}

const MINIMAP_W = 160
const MINIMAP_H = 100

export default function Minimap({cy, tokens, width = MINIMAP_W, height = MINIMAP_H}: Props) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const dragging = useRef(false)

  // Resolve node dot color from theme tokens (falls back to neutral grey).
  const getDotColor = useCallback((type: string): string => {
    if (!tokens) {
      const fallback: Record<string, string> = {
        ACTION: '#3b82f6', LOOP: '#f59e0b', CONDITION: '#10b981',
        SUBFLOW: '#a855f7', ERROR_HANDLER: '#ef4444', COMMENT: '#6b7280',
        VARIABLE: '#ec4899', WAIT: '#14b8a6',
      }
      return fallback[type] ?? '#6b7280'
    }
    const map: Record<string, keyof GraphTokenColors> = {
      ACTION: 'blockAction', LOOP: 'blockLoop', CONDITION: 'blockCondition',
      SUBFLOW: 'blockSubflow', ERROR_HANDLER: 'blockError', COMMENT: 'blockComment',
      VARIABLE: 'blockVariable', WAIT: 'blockWait',
    }
    return map[type] ? tokens[map[type]] : tokens.blockComment
  }, [tokens])

  const drawMinimap = useCallback(() => {
    const canvas = canvasRef.current
    if (!canvas || !cy) return

    const ctx = canvas.getContext('2d')
    if (!ctx) return

    const dpr = window.devicePixelRatio || 1
    canvas.width = width * dpr
    canvas.height = height * dpr
    ctx.scale(dpr, dpr)

    ctx.clearRect(0, 0, width, height)
    ctx.fillStyle = 'rgba(0,0,0,0.3)'
    ctx.fillRect(0, 0, width, height)

    const nodes = cy.nodes()
    if (nodes.length === 0) return

    const bounds = nodes.boundingBox()
    const graphW = bounds.x2 - bounds.x1
    const graphH = bounds.y2 - bounds.y1
    if (graphW === 0 || graphH === 0) return

    const pad = 10
    const scaleX = (width - pad * 2) / graphW
    const scaleY = (height - pad * 2) / graphH
    const scale = Math.min(scaleX, scaleY)
    const offsetX = pad + ((width - pad * 2) - graphW * scale) / 2
    const offsetY = pad + ((height - pad * 2) - graphH * scale) / 2

    nodes.forEach((node: any) => {
      const pos = node.position()
      const x = (pos.x - bounds.x1) * scale + offsetX - 2
      const y = (pos.y - bounds.y1) * scale + offsetY - 2
      ctx.fillStyle = getDotColor(node.data('type'))
      ctx.fillRect(x, y, 4, 4)
    })

    const vp = cy.extent()
    const vpX1 = (vp.x1 - bounds.x1) * scale + offsetX
    const vpY1 = (vp.y1 - bounds.y1) * scale + offsetY
    const vpX2 = (vp.x2 - bounds.x1) * scale + offsetX
    const vpY2 = (vp.y2 - bounds.y1) * scale + offsetY

    ctx.strokeStyle = 'rgba(99, 102, 241, 0.6)'
    ctx.lineWidth = 1.5
    ctx.strokeRect(vpX1, vpY1, vpX2 - vpX1, vpY2 - vpY1)
    ctx.fillStyle = 'rgba(99, 102, 241, 0.08)'
    ctx.fillRect(vpX1, vpY1, vpX2 - vpX1, vpY2 - vpY1)
  }, [cy, width, height, getDotColor])

  // Pan the main graph so the clicked/dragged minimap point is centered.
  const panToMinimapPoint = useCallback((clientX: number, clientY: number) => {
    if (!cy) return
    const canvas = canvasRef.current
    if (!canvas) return

    const rect = canvas.getBoundingClientRect()
    const x = clientX - rect.left
    const y = clientY - rect.top

    const nodes = cy.nodes()
    if (nodes.length === 0) return

    const bounds = nodes.boundingBox()
    const graphW = bounds.x2 - bounds.x1
    const graphH = bounds.y2 - bounds.y1
    if (graphW === 0 || graphH === 0) return

    const pad = 10
    const scaleX = (width - pad * 2) / graphW
    const scaleY = (height - pad * 2) / graphH
    const scale = Math.min(scaleX, scaleY)
    const offsetX = pad + ((width - pad * 2) - graphW * scale) / 2
    const offsetY = pad + ((height - pad * 2) - graphH * scale) / 2

    const graphX = (x - offsetX) / scale + bounds.x1
    const graphY = (y - offsetY) / scale + bounds.y1

    // Set absolute pan so the clicked graph point is centered in the viewport.
    cy.pan({
      x: cy.width() / 2 - graphX * cy.zoom(),
      y: cy.height() / 2 - graphY * cy.zoom(),
    })
    drawMinimap()
  }, [cy, width, height, drawMinimap])

  useEffect(() => {
    if (!cy) return
    cy.on('viewport resize', drawMinimap)
    drawMinimap()
    return () => { cy.off('viewport resize', drawMinimap) }
  }, [cy, drawMinimap])

  // Window-level drag handlers so dragging outside the canvas still works.
  useEffect(() => {
    const onMouseMove = (e: MouseEvent) => {
      if (dragging.current) panToMinimapPoint(e.clientX, e.clientY)
    }
    const onMouseUp = () => { dragging.current = false }
    window.addEventListener('mousemove', onMouseMove)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      window.removeEventListener('mousemove', onMouseMove)
      window.removeEventListener('mouseup', onMouseUp)
    }
  }, [panToMinimapPoint])

  const handleMouseDown = useCallback((e: React.MouseEvent<HTMLCanvasElement>) => {
    dragging.current = true
    panToMinimapPoint(e.clientX, e.clientY)
  }, [panToMinimapPoint])

  return (
    <canvas
      ref={canvasRef}
      style={{width, height}}
      className="absolute bottom-3 right-3 rounded border border-border-default bg-surface-1/80 cursor-pointer"
      onMouseDown={handleMouseDown}
    />
  )
}
