import {defineConfig} from 'vitest/config'
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
    globals: true,
    setupFiles: ['./src/testing/setup.ts'],
    exclude: ['node_modules', 'dist', 'e2e', 'playwright.config.ts'],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/stores/**', 'src/lib/**', 'src/hooks/**', 'src/components/**', 'src/services/**'],
      // Floor, not a target. Set just under the current measured numbers so an
      // erosion fails the run while ordinary work does not; raise as coverage
      // climbs. Previously `include` was configured but nothing enforced it, so
      // `test:coverage` reported numbers no gate ever read.
      // Measured at the time of writing: statements 50.86, branches 43.80,
      // functions 48.50, lines 52.11. Each floor sits just below its actual so
      // erosion fails the run while ordinary work does not. Raise as they climb.
      thresholds: {
        statements: 49,
        branches: 42,
        functions: 47,
        lines: 50,
      },
    },
  },
})
