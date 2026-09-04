import {describe, it, expect} from 'vitest'
import {readdirSync, readFileSync, statSync} from 'node:fs'
import {join} from 'node:path'

// Anti-regression guard for the i18n rollout.
//
// The app shipped a full i18n setup that only 26 of 473 files actually used, so
// English kept getting hardcoded into new components faster than it was being
// extracted. This test locks in each directory as it is converted: once a
// namespace lands here, a new component in it that renders raw English fails the
// suite instead of quietly widening the gap.
//
// ADD A DIRECTORY HERE AS SOON AS ITS NAMESPACE IS COMPLETE — that is the whole
// point. Do not add one "to do later".
const I18N_COMPLETE_DIRS = [
  'src/components/findings',
  'src/components/auth',
  'src/components/shared',
  'src/components/layout',
  'src/components/sidebar',
  'src/components/chat',
  'src/components/settings',
]

// Files with no user-facing copy: barrels, pure logic/data, and presentational
// components that render only values passed to them. Each entry is a deliberate
// statement that the file has nothing to translate.
const NO_USER_FACING_COPY = new Set([
  'src/components/findings/index.ts',
  // Only logs to the developer console; no rendered copy.
  'src/components/findings/hooks/useRelatedFindings.ts',
  // Route guard: renders a spinner or its children, no copy of its own.
  'src/components/auth/ProtectedRoute.tsx',
  // Pure string/hash helpers.
  'src/components/auth/authHash.ts',

  // --- src/components/settings ---
  // Re-export barrel; no rendered copy of its own.
  'src/components/settings/index.ts',

  // --- src/components/shared ---
  // Presentational primitives: every string they render is passed in by the
  // caller, which is where it gets translated.
  'src/components/shared/index.ts',
  'src/components/shared/Avatar.tsx',
  'src/components/shared/Button.tsx',
  'src/components/shared/Checkbox.tsx',
  'src/components/shared/Divider.tsx',
  'src/components/shared/Dropdown.tsx',
  'src/components/shared/EmptyState.tsx',
  'src/components/shared/Icon.tsx',
  'src/components/shared/IconButton.tsx',
  'src/components/shared/Input.tsx',
  'src/components/shared/NumberField.tsx',
  'src/components/shared/PatchPreviewText.tsx',
  'src/components/shared/Portal.tsx',
  'src/components/shared/SegmentedControl.tsx',
  'src/components/shared/Skeleton.tsx',
  'src/components/shared/Switch.tsx',
  'src/components/shared/Tabs.tsx',
  'src/components/shared/Textarea.tsx',
  'src/components/shared/Tooltip.tsx',
  // Kbd renders KEYCAP names (Shift, Enter, ⌘). Those mirror what is physically
  // printed on the user's keyboard, so they are deliberately not translated.
  'src/components/shared/Kbd.tsx',

  // --- src/components/layout & src/components/sidebar ---
  'src/components/layout/index.ts',
  'src/components/sidebar/index.ts',
  // Renders only names taken from the parsed flow document.
  'src/components/layout/Breadcrumbs.tsx',
  // Pure drag affordance and a pure router switch — no copy.
  'src/components/layout/PaneDivider.tsx',
  'src/components/layout/SystemViewRouter.tsx',
  // Chip labels arrive pre-translated from Sidebar's blockTypes lookup.
  'src/components/sidebar/FilterChips.tsx',
  // Renders block labels from the document; its only strings are key names.
  'src/components/sidebar/FlowTree.tsx',
  'src/components/sidebar/TreeNode.tsx',
  // Pure search/filter logic, no rendered copy.
  'src/components/sidebar/hooks/useSidebarSearch.ts',

  // --- src/components/chat ---
  'src/components/chat/index.ts',
  // Renders only values from the store / props.
  'src/components/chat/StreamingBubble.tsx',
  'src/components/chat/SuggestedPrompts.tsx',
  // Data/state plumbing with no rendered copy.
  'src/components/chat/hooks/useChatConversations.ts',
  'src/components/chat/hooks/useChatRequestBuilder.ts',
  'src/components/chat/hooks/useChatThreads.ts',
  'src/components/chat/hooks/useProviderSetup.ts',
])

// Hook for function components; the i18next instance for class components
// (ErrorBoundary), which cannot use hooks.
function usesI18n(src: string): boolean {
  return src.includes('useTranslation') || src.includes("from '@/i18n'")
}

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) {
      out.push(...sourceFiles(full))
      continue
    }
    if (!/\.tsx?$/.test(entry)) continue
    if (/\.test\.tsx?$/.test(entry)) continue
    out.push(full)
  }
  return out
}

// findRawCopy returns the English literals a file still renders directly.
//
// The import check above only proves a file CAN translate — a component that
// calls t() once passed while rendering a dozen raw strings, which is exactly
// how ~20 literals drifted back into src/components/chat after that directory
// was marked complete. This scans the two places user-visible copy actually
// appears:
//
//   1. JSX text nodes:            <span>Try again</span>
//   2. Human-readable attributes: aria-label="Close search", title={`Go to ${x}`}
//
// Deliberately NOT flagged: single capitalised words with no space (product
// names, keycaps, enum-ish labels), anything containing `t(` or `i18n.`, and
// values that are obviously identifiers (no lowercase letters, or dotted /
// snake_case / kebab-case tokens).
const HUMAN_ATTRS = ['aria-label', 'title', 'placeholder', 'alt', 'aria-description']

// Directories whose raw-copy scan is enforced (see the note on the test below).
const RAW_COPY_ENFORCED = new Set(['src/components/chat'])

function findRawCopy(src: string): string[] {
  const out: string[] = []
  const looksLikeProse = (s: string) => /[a-z]/.test(s) && /\s/.test(s) && !/^[\w.-]+$/.test(s)

  // JSX text nodes between tags, e.g. `>Try again<`.
  for (const m of src.matchAll(/>\s*([A-Z][^<>{}\n]{3,})\s*</g)) {
    const text = m[1].trim()
    if (looksLikeProse(text)) out.push(text)
  }

  // Human-readable attributes given a literal or a template literal.
  const attrs = HUMAN_ATTRS.join('|')
  const re = new RegExp(`\\b(?:${attrs})=(?:"([^"]{4,})"|\\{\`([^\`]{4,})\`\\})`, 'g')
  for (const m of src.matchAll(re)) {
    const text = (m[1] ?? m[2]).trim()
    if (text.includes('t(') || text.includes('i18n.')) continue
    if (looksLikeProse(text)) out.push(text)
  }

  return [...new Set(out)]
}

describe('i18n coverage', () => {
  for (const dir of I18N_COMPLETE_DIRS) {
    it(`${dir} routes all user-facing copy through i18n`, () => {
      const offenders = sourceFiles(dir).filter(f => !NO_USER_FACING_COPY.has(f) && !usesI18n(readFileSync(f, 'utf8')))
      expect(offenders).toEqual([])
    })

    // Enforced per-directory. The scan found real drift in every directory on
    // I18N_COMPLETE_DIRS — the import-only check above was never able to see
    // it — but clearing ~200 literals across findings/auth/shared/sidebar/
    // settings is its own piece of work. Chat is clean and locked in here;
    // ADD A DIRECTORY TO RAW_COPY_ENFORCED AS SOON AS YOU CLEAR IT.
    it.skipIf(!RAW_COPY_ENFORCED.has(dir))(`${dir} renders no raw English literals`, () => {
      const offenders: string[] = []
      for (const f of sourceFiles(dir)) {
        if (NO_USER_FACING_COPY.has(f)) continue
        for (const text of findRawCopy(readFileSync(f, 'utf8'))) {
          offenders.push(`${f}: ${text}`)
        }
      }
      expect(offenders).toEqual([])
    })
  }

  it('every allowlisted file still exists', () => {
    // Keeps the allowlist honest: a renamed or deleted file must not leave a
    // stale entry that silently exempts a future file at the same path.
    const missing = [...NO_USER_FACING_COPY].filter(f => {
      try {
        statSync(f)
        return false
      } catch {
        return true
      }
    })
    expect(missing).toEqual([])
  })
})
