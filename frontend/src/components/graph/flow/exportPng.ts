import {getViewportForBounds, type Rect} from '@xyflow/react'
import {logger} from '@/lib/logger'

/** Hard cap on the exported image, so a huge graph cannot ask the browser for a gigapixel canvas. */
const MAX_SIDE = 4096
const PADDING = 40

/** Overlays that sit inside the flow but do not belong in the picture. */
const OVERLAY_SELECTOR = '.react-flow__minimap, .react-flow__controls, .react-flow__attribution, .react-flow__panel'

function cssColor(name: string, fallback: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return value || fallback
}

/**
 * Rasterize the whole graph — not just the part on screen — by rendering the viewport element at
 * the transform that fits `bounds` (the caller gets those from `useReactFlow().getNodesBounds`),
 * the way React Flow's own download example does it. `html-to-image` is loaded on demand so it
 * stays out of the graph chunk.
 */
export async function exportFlowPng(container: HTMLElement | null, bounds: Rect, fileName: string): Promise<void> {
  const viewport = container?.querySelector<HTMLElement>('.react-flow__viewport')
  if (!viewport) return
  if (bounds.width === 0 || bounds.height === 0) return

  const scale = Math.min(1, MAX_SIDE / (bounds.width + PADDING * 2), MAX_SIDE / (bounds.height + PADDING * 2))
  const width = Math.ceil((bounds.width + PADDING * 2) * scale)
  const height = Math.ceil((bounds.height + PADDING * 2) * scale)
  const {x, y, zoom} = getViewportForBounds(bounds, width, height, 0.05, 2, `${PADDING}px`)

  try {
    const {toPng} = await import('html-to-image')
    const dataUrl = await toPng(viewport, {
      width,
      height,
      pixelRatio: 2,
      backgroundColor: cssColor('--surface-0', '#ffffff'),
      style: {
        width: `${width}px`,
        height: `${height}px`,
        transform: `translate(${x}px, ${y}px) scale(${zoom})`,
      },
      filter: node => !(node instanceof Element && node.matches(OVERLAY_SELECTOR)),
    })
    const link = document.createElement('a')
    link.href = dataUrl
    link.download = fileName
    link.click()
  } catch (err) {
    logger.warn('graph png export failed', err)
  }
}
