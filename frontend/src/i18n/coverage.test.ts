import {describe, it, expect} from 'vitest'
import {readdirSync, readFileSync, statSync} from 'node:fs'
import {join} from 'node:path'

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
  'src/components/inspector',
  'src/components/analyzer',
  'src/components/search',
  'src/components/collaboration',
  'src/components/library',
  'src/components/flow',
]

const NO_USER_FACING_COPY = new Set([
  'src/components/findings/index.ts',
  'src/components/findings/hooks/useRelatedFindings.ts',
  'src/components/auth/ProtectedRoute.tsx',
  'src/components/auth/authHash.ts',
  'src/components/auth/SectionCard.tsx',
  'src/components/auth/useActiveSection.ts',
  'src/components/auth/authNotice.ts',

  'src/components/settings/providers/ProviderSection.tsx',
  'src/components/settings/sections.ts',
  'src/components/settings/SettingRow.tsx',
  'src/components/settings/azure/connectionForm.ts',
  'src/components/settings/azure/modelForm.ts',
  'src/components/settings/azure/registryView.ts',
  'src/components/settings/OrganizationsPanel/helpers.ts',

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
  'src/components/shared/Kbd.tsx',
  'src/components/shared/GithubMark.tsx',

  'src/components/layout/Breadcrumbs.tsx',
  'src/components/layout/PaneDivider.tsx',
  'src/components/layout/SystemViewRouter.tsx',
  'src/components/sidebar/FilterChips.tsx',
  'src/components/sidebar/FlowTree.tsx',
  'src/components/sidebar/TreeNode.tsx',
  'src/components/sidebar/hooks/useSidebarSearch.ts',

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

  'src/components/dashboard/home/activity.ts',

  'src/components/inspector/index.ts',
  'src/components/inspector/DetailsHeader.tsx',
  'src/components/inspector/ChildrenList.tsx',
  'src/components/inspector/CollapsibleSection.tsx',
  'src/components/inspector/StatCard.tsx',
  'src/components/inspector/VariableChips.tsx',

  'src/components/library/LibrarySkeleton.tsx',
  'src/components/library/useLibraryWriteErrorToast.ts',

  'src/components/flow/BlockConnector.tsx',
  'src/components/flow/index.ts',
  'src/components/flow/subflowMap/layout.ts',
  'src/components/flow/subflowMap/model.ts',
  'src/components/flow/subflowMap/roles.ts',
  'src/components/flow/subflowMap/useSubflowMapGraph.ts',
  'src/components/dashboard/home/SkeletonDashboard.tsx',

  'src/components/chat/hooks/stream-engine/accumulator.ts',
  'src/components/chat/hooks/stream-engine/types.ts',
  'src/components/chat/hooks/useChatStreamEngine.ts',
  'src/components/chat/aitab/SelectionContextBar.tsx',
  'src/components/chat/aitab/SelectionPromptTemplates.tsx',
  'src/components/chat/aitab/useMessageSearch.ts',
  'src/components/chat/aitab/useStreamFooter.tsx',
  'src/components/chat/aitab/useSuggestedPrompts.ts',
  'src/components/chat/approval/preview.ts',
  'src/components/dashboard/analytics/SkeletonAnalytics.tsx',
  'src/components/dashboard/home/HomeChartCards.ts',
  'src/components/dashboard/analytics/ChartSection.tsx',

  'src/components/admin/AdminSection.tsx',
  'src/components/admin/auditActions.ts',
  'src/components/admin/roles.ts',
  'src/components/admin/hooks/useAdminQuery.ts',
  'src/components/admin/hooks/useUserStats.ts',
  'src/components/admin/hooks/useAuditActions.ts',

  'src/components/chat/index.ts',
  'src/components/chat/StreamingBubble.tsx',
  'src/components/chat/SuggestedPrompts.tsx',
  'src/components/chat/hooks/useChatConversations.ts',
  'src/components/chat/hooks/useChatRequestBuilder.ts',
  'src/components/chat/hooks/useProviderSetup.ts',
  'src/components/chat/hooks/useSelectedBlock.ts',
  'src/components/chat/ChatStreamProvider.tsx',
  'src/components/chat/chatStreamContext.ts',
  'src/components/chat/providerGlyphs.ts',
])

function usesI18n(src: string): boolean {
  return src.includes('useTranslation') || src.includes("from '@/i18n'")
}

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
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

const HUMAN_ATTRS = ['aria-label', 'title', 'placeholder', 'alt', 'aria-description']

const RAW_COPY_ENFORCED = new Set(I18N_COMPLETE_DIRS)

function findRawCopy(src: string): string[] {
  const out: string[] = []
  const looksLikeProse = (s: string) => /[a-z]/.test(s) && /\s/.test(s) && !/^[\w.-]+$/.test(s)

  for (const m of src.matchAll(/>\s*([A-Z][^<>{}\n]{3,})\s*</g)) {
    const text = m[1]!.trim()
    if (looksLikeProse(text)) out.push(text)
  }

  const attrs = HUMAN_ATTRS.join('|')
  const re = new RegExp(`\\b(?:${attrs})=(?:"([^"]{4,})"|\\{\`([^\`]{4,})\`\\})`, 'g')
  for (const m of src.matchAll(re)) {
    const text = (m[1] ?? m[2])!.trim()
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
