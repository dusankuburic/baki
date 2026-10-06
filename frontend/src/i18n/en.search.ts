// Search namespace — the command palette, the global search overlay and the
// keyboard-shortcuts help dialog (src/components/search). English values must
// match the pre-i18n strings EXACTLY where a test asserts on them.
export const enSearch = {
  palette: {
    dialogAria: 'Command palette',
    placeholder: 'Type a command...',
    inputAria: 'Type a command',
    empty: 'No commands found',
  },
  overlay: {
    dialogAria: 'Search',
    placeholder: 'Search across all subflows and properties...',
    inputAria: 'Search across all subflows and properties',
    closeAria: 'Close search',
    scopeThisFlow: 'This Flow',
    scopeAllFlows: 'All Flows',
    filterLabel: 'Filter:',
    clear: 'Clear',
    resultsAria: 'Search results',
    unknownSubflow: 'Unknown Subflow',
    noMatches: 'No matches found for "{{query}}"',
    noMatchesScoped: 'No matches found for "{{query}}" in selected types',
    hintAllFlows: 'Type to search across all your flows...',
    hintThisFlow: 'Type to search across the entire flow...',
    hintSub: 'Search properties, variables, and comments',
    resultsCapped: '50+ results (refine query)',
    results: '{{count}} results',
    navigate: 'Navigate',
    open: 'Open',
    close: 'Close',
    // Block-type filter chips (order and set mirror the BlockType union).
    types: {
      action: 'Action',
      condition: 'If',
      loop: 'Loop',
      switch: 'Switch',
      variable: 'Var',
      errorHandler: 'Error',
      comment: 'Comment',
    },
  },
  shortcuts: {
    title: 'Keyboard Shortcuts',
    searchPlaceholder: 'Search shortcuts...',
  },
}
