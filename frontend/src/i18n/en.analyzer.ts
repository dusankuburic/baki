// Analyzer namespace — the rule reference catalogue and the related-rules
// dependency map (src/components/analyzer). English values must match the
// pre-i18n strings EXACTLY where a test asserts on them.
export const enAnalyzer = {
  reference: {
    title: 'Rule Reference',
    summary: '{{rules}} rules across {{categories}} categories · {{autoFixable}} with auto-fix',
    searchPlaceholder: 'Search rules...',
    allCategories: 'All categories',
    autofix: 'auto-fix',
    category: 'Category: {{category}}',
    severity: 'Severity: {{severity}}',
    confidence: 'Confidence: {{confidence}}',
    noMatches: 'No rules match "{{query}}"',
    loadFailed: 'Failed to load rules.',
    previewCount: '{{count}} in preview',
    libraryKind: 'Across flows',
    libraryKindHint: 'Compares the flows of a folder with each other; runs when a folder is analyzed.',
  },
  // Preview rules (core/analyzer Stage): reported, but never a gate.
  stage: {
    preview: 'Preview',
    previewRuleHint:
      'Preview rule: its findings are reported but never fail a CI gate or a policy, and do not count toward the health score.',
    previewFindingHint:
      'Found by a preview rule: reported, but it never fails a CI gate or a policy and does not count toward the health score.',
  },
  dependencies: {
    back: '← Back',
    title: 'Related rules',
    summary: '{{links}} links · {{rules}} rules',
    refresh: 'Refresh',
    // Fallback help text when the backend does not send `description`; it says
    // the same thing the backend does.
    editorialNote:
      'Hand-maintained editorial map of rules whose findings are worth reviewing together. ' +
      'It is not derived from the analyzer and has no effect on evaluation order or results.',
    cycleWarning: 'Circular reference in the related-rules map:',
    noRelated: 'No related rules listed.',
    legendInCycle: 'in a cycle',
    legendRule: 'rule',
  },
}
