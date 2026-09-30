import {defineConfig} from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

// Test files that must run in the standard `forks` pool: they depend on what
// the `vmThreads` pool isolates away — Node's own web streams under fake
// timers (the SSE client), jiti patching `require` (the Tailwind config
// loader), and a redefinable `window.location` (jsdom's is non-configurable
// inside a VM context). A new test that fails only under vmThreads belongs
// here, not in a workaround.
const REALM_SENSITIVE = [
  'src/api/client.sse.test.ts',
  'src/lib/tailwindTokens.test.ts',
  'src/components/auth/LoginForm.sso.test.tsx',
]

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
    // vmThreads loads jsdom once per worker and gives each test file a fresh
    // VM context (module state stays per file, as in `forks`). The default
    // pool re-requires jsdom for every file — ~0.7 s of module loading each,
    // most of the suite's time — so this runs it several times faster.
    projects: [
      {extends: true, test: {name: 'unit', pool: 'vmThreads', exclude: REALM_SENSITIVE}},
      {extends: true, test: {name: 'realm', pool: 'forks', include: REALM_SENSITIVE}},
    ],
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
