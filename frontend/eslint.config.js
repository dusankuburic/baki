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
    },
  },
]
