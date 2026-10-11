import {defineConfig} from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

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
    projects: [
      {extends: true, test: {name: 'unit', pool: 'vmThreads', exclude: REALM_SENSITIVE}},
      {extends: true, test: {name: 'realm', pool: 'forks', include: REALM_SENSITIVE}},
    ],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/stores/**', 'src/lib/**', 'src/hooks/**', 'src/components/**', 'src/services/**'],
      thresholds: {
        statements: 49,
        branches: 42,
        functions: 47,
        lines: 50,
      },
    },
  },
})
