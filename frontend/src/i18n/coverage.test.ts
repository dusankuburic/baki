import {describe, it, expect} from 'vitest'
import {readdirSync, readFileSync, statSync} from 'node:fs'
import {join} from 'node:path'

// Anti-regression guard for the i18n rollout.
//
// The app shipped a full i18n setup that only 26 of 473 files actually used, so
// English kept getting hardcoded into new components faster than it was being
// extracted. This test locks in each directory as it is converted: once a
// namespace lands here, a new component in it that does not use i18n at all
// fails the suite, and so does one that renders raw English literals (the
// stricter scan below, enforced for every directory on the list).
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
  'src/components/charts',
  'src/components/dashboard/home',
  'src/components/dashboard/analytics',
  'src/components/admin',
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
  // Profile section chrome: renders only the copy its callers pass in.
  'src/components/auth/SectionCard.tsx',
  // Scroll-position tracking for the profile's section nav; no copy.
  'src/components/auth/useActiveSection.ts',
  // sessionStorage plumbing for the sign-in screen's post-logout note.
  'src/components/auth/authNotice.ts',

  // --- src/components/settings ---
  // Re-export barrel; no rendered copy of its own.
  'src/components/settings/index.ts',
  // Section registry: ids, icons, lazy loaders and pure search matching.
  'src/components/settings/sections.ts',
  // Settings-row chrome: renders only the copy its callers pass in.
  'src/components/settings/SettingRow.tsx',
  // Pure form logic of the Azure AI Foundry modals: validators return
  // error codes and i18n keys, the callers translate them.
  'src/components/settings/azure/connectionForm.ts',
  'src/components/settings/azure/modelForm.ts',
  // Pure view logic of AI Accounts' registry (ownership, limits, defaults,
  // duplicates, focus targets): no copy, the components translate.
  'src/components/settings/azure/registryView.ts',

  // --- src/components/shared ---
  // Presentational primitives: every string they render is passed in by the
  // caller, which is where it gets translated.
  'src/components/shared/index.ts',
  'src/components/shared/Avatar.tsx',
  'src/components/shared/Badge.tsx',
  'src/components/shared/Button.tsx',
  'src/components/shared/Checkbox.tsx',
  'src/components/shared/Divider.tsx',
  'src/components/shared/Dropdown.tsx',
  'src/components/shared/EmptyState.tsx',
  'src/components/shared/FormField.tsx',
  'src/components/shared/Icon.tsx',
  'src/components/shared/IconButton.tsx',
  'src/components/shared/Input.tsx',
  'src/components/shared/NumberField.tsx',
  'src/components/shared/PatchPreviewText.tsx',
  'src/components/shared/Portal.tsx',
  'src/components/shared/SegmentedControl.tsx',
  'src/components/shared/Select.tsx',
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

  // --- src/components/charts ---
  // The chart foundation renders only the names and values its callers pass
  // in (already translated); the rest is theme, geometry and formatting.
  'src/components/charts/index.ts',
  'src/components/charts/chartTypes.ts',
  'src/components/charts/axes.ts',
  'src/components/charts/useChartTheme.ts',
  'src/components/charts/useReducedMotion.ts',
  'src/components/charts/ChartFigure.tsx',
  'src/components/charts/ChartLegend.tsx',
  'src/components/charts/ChartTooltip.tsx',
  'src/components/charts/Sparkline.tsx',
  'src/components/charts/Meter.tsx',
  'src/components/charts/HBarChart.tsx',
  'src/components/charts/ColumnChart.tsx',
  'src/components/charts/StackedAreaChart.tsx',
  'src/components/charts/LineTrendChart.tsx',
  'src/components/charts/ScatterPlot.tsx',

  // --- src/components/dashboard ---
  // Icon lookup and id humanising for audit actions; the labels themselves
  // come from the translated table the callers pass in.
  'src/components/dashboard/home/activity.ts',
  // Loading shapes with no copy.
  'src/components/dashboard/home/SkeletonDashboard.tsx',
  'src/components/dashboard/analytics/SkeletonAnalytics.tsx',
  // Re-exports the recharts-backed Home cards for HomeDashboard's lazy import.
  'src/components/dashboard/home/HomeChartCards.ts',
  // Section chrome: renders only the copy its callers pass in.
  'src/components/dashboard/analytics/ChartSection.tsx',

  // --- src/components/admin ---
  // Section chrome and skeleton rows: render only the copy their callers pass in.
  'src/components/admin/AdminSection.tsx',
  // Pure grouping/tone helpers for audit action ids; the labels come from
  // the caller's translated table.
  'src/components/admin/auditActions.ts',
  // Role constants and a label lookup that takes the caller's `t`.
  'src/components/admin/roles.ts',
  // Data plumbing with no rendered copy (the hooks that toast use i18n).
  'src/components/admin/hooks/useAdminQuery.ts',
  'src/components/admin/hooks/useUserStats.ts',
  'src/components/admin/hooks/useAuditActions.ts',

  // --- src/components/chat ---
  'src/components/chat/index.ts',
  // Renders only values from the store / props.
  'src/components/chat/StreamingBubble.tsx',
  'src/components/chat/SuggestedPrompts.tsx',
  // Data/state plumbing with no rendered copy.
  'src/components/chat/hooks/useChatConversations.ts',
  'src/components/chat/hooks/useChatRequestBuilder.ts',
  'src/components/chat/hooks/useProviderSetup.ts',
  'src/components/chat/hooks/useSelectedBlock.ts',
  // The app-wide stream host and its context: render only their children.
  'src/components/chat/ChatStreamProvider.tsx',
  'src/components/chat/chatStreamContext.ts',
])

// Hook for function components; the i18next instance for class components
// (ErrorBoundary), which cannot use hooks.
function usesI18n(src: string): boolean {
  return src.includes('useTranslation') || src.includes("from '@/i18n'")
}

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    // Normalize to forward slashes: join() yields backslashes on Windows, and
    // the allowlist below is written with forward slashes.
    const full = join(dir, entry).replaceAll('\\', '/')
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

// Directories whose raw-copy scan is enforced. Every directory on
// I18N_COMPLETE_DIRS is now clear, so the scan gates them all: a new component
// that renders raw English in any of them fails here.
const RAW_COPY_ENFORCED = new Set(I18N_COMPLETE_DIRS)

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

    // Enforced for every directory in I18N_COMPLETE_DIRS (see
    // RAW_COPY_ENFORCED). The skipIf stays so a directory can be added to the
    // list above while its literals are still being extracted.
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
