import {defineConfig} from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

function vendorChunk(id: string): string | undefined {
  if (!id.includes('node_modules')) return undefined
  const sep = /[\\/]/.source
  const inPkg = (names: string[]) =>
    names.some(n => new RegExp(`node_modules${sep}${n.replace('/', sep)}${sep}`).test(id))

  if (inPkg(['react', 'react-dom', 'scheduler', 'use-sync-external-store'])) return 'react-vendor'
  if (inPkg(['clsx'])) return 'react-vendor'
  if (inPkg(['@xyflow/react', '@xyflow/system', 'dagre', 'd3-zoom', 'd3-drag', 'd3-selection'])) return 'graph'
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
    sourcemap: 'hidden',
    modulePreload: false,
    rollupOptions: {
      output: {
        manualChunks: vendorChunk,
      },
    },
  },
})
