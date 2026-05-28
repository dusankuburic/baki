import js from '@eslint/js'
import tsParser from '@typescript-eslint/parser'
import tsPlugin from '@typescript-eslint/eslint-plugin'
import reactHooks from 'eslint-plugin-react-hooks'

export default [
  js.configs.recommended,
  {
    ignores: ['dist/**'],
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsParser,
      parserOptions: {
        ecmaVersion: 'latest',
        sourceType: 'module',
      },
    },
    plugins: {
      '@typescript-eslint': tsPlugin,
      'react-hooks': reactHooks,
    },
    rules: {
      ...tsPlugin.configs.recommended.rules,
      ...reactHooks.configs.recommended.rules,
      '@typescript-eslint/no-explicit-any': 'warn',
      '@typescript-eslint/no-unused-vars': ['warn', {argsIgnorePattern: '^_', varsIgnorePattern: '^_'}],
      'no-console': ['warn', {allow: ['warn', 'error']}],
      'prefer-const': 'error',
      'no-undef': 'off', // TypeScript handles this
      // Intentional patterns: updating ref.current during render is a valid stale-closure workaround
      'react-hooks/refs': 'warn',
      // setState in effect body is sometimes intentional (conditional initialization)
      'react-hooks/set-state-in-effect': 'warn',
      // Keep platform-specific code isolated: only the platform adapters may import Tauri APIs.
      // (Dynamic import() is not flagged by this rule, so guarded lazy imports remain allowed.)
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['@tauri-apps/*'],
          message: 'Import Tauri APIs only inside src/platform/adapters — use the PlatformAdapter abstraction elsewhere.',
        }],
      }],
    },
  },
  {
    // The platform adapters are the designated home for Tauri-specific imports.
    files: ['src/platform/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-imports': 'off',
    },
  },
]
