import {defineConfig} from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/testing/setup.ts'],
    // Keep explicit vitest imports — no globals injection needed
    globals: false,
  },
})
