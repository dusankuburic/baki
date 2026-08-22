import {defineConfig} from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

// manualChunks uses FUNCTION form deliberately. The previous object form let
// rollup place shared react ecosystem modules (react-dom/client, scheduler)
// inside the charts chunk, which made the ENTRY chunk statically import the
// 600 kB charts bundle — the lazy() boundaries never got a chance to matter.
// Function form claims the react family first, so heavy vendor groups are
// only reachable through their intended lazy importers.
//
// The markdown family (react-markdown, remark-gfm, micromark, …) is NOT
// claimed: its only importer is chat/MessageBubble inside the lazy AITab
// chunk, so rollup co-locates it there — loaded at AI-tab open, never eager.
function vendorChunk(id: string): string | undefined {
  if (!id.includes('node_modules')) return undefined
  const sep = /[\\/]/.source
  const inPkg = (names: string[]) =>
    names.some(n => new RegExp(`node_modules${sep}${n.replace('/', sep)}${sep}`).test(id))

  // React ecosystem FIRST — claims shared modules away from every other group.
  if (inPkg(['react', 'react-dom', 'scheduler', 'use-sync-external-store'])) return 'react-vendor'
  // clsx is imported by ~40 eager components AND by recharts; without an
  // explicit claim rollup co-locates it with recharts, which drags the whole
  // charts chunk into the eager entry graph through the shared binding.
  if (inPkg(['clsx'])) return 'react-vendor'
  if (inPkg(['cytoscape', 'cytoscape-dagre', 'dagre'])) return 'graph'
  // recharts + its exclusive dependency closure.
  if (
    inPkg([
      'recharts',
      'victory-vendor',
      'lodash-es',
      'd3-quadtree',
      'd3-shape',
      'd3-path',
      'd3-scale',
      'd3-interpolate',
      'd3-format',
      'd3-time',
      'd3-time-format',
      'd3-array',
    ])
  )
    return 'charts'
  return undefined
}

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  build: {
    // 'hidden' emits .map files without a sourceMappingURL comment in the
    // bundles: nothing for browsers to auto-download (no perf/dev-info leak
    // to users), but the maps can be uploaded to an error-tracking service
    // to symbolicate production stack traces.
    sourcemap: 'hidden',
    rollupOptions: {
      output: {
        manualChunks: vendorChunk,
      },
    },
  },
})
