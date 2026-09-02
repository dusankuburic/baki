// English source resources — the single source of truth for UI strings.
// Structured by namespace (common / shell / findings / auth) so components
// import only what they need and later locales can be added per-namespace.
//
// KEY CONVENTIONS:
//   - Keys mirror the component concept, not the English words
//     (findings.search.placeholder, not findings.searchFindings).
//   - English values must match the pre-i18n strings EXACTLY where a test
//     or e2e asserts on them; those tests keep passing untranslated.
//   - Plurals use i18next's `_one`/`_other` suffixes with a {{count}}
//     interpolation variable.
//   - Do NOT mix string leaves and nested objects at the same level: the
//     i18next TS type-flattener silently drops the nested object's keys
//     (see severity → badge restructure). Keep each level homogeneous.

export const common = {
  cancel: 'Cancel',
  save: 'Save',
  close: 'Close',
  retry: 'Retry',
  loading: 'Loading…',
  error: 'Something went wrong',
}

export const shell = {
  nav: {
    home: 'Home',
    analytics: 'Analytics',
    library: 'Library',
    profile: 'Profile',
    admin: 'Admin',
  },
  toolbar: {
    goBack: 'Go Back',
    goForward: 'Go Forward',
    zoomIn: 'Zoom in',
    zoomOut: 'Zoom out',
    fitToScreen: 'Fit to screen',
    exportGraphPng: 'Export graph as PNG',
    newComparison: 'New Comparison',
    complexityMap: 'Complexity Map',
    fullscreen: 'Fullscreen',
    reimport: 'Re-import flow',
    reimporting: 'Re-importing…',
    exportPdf: 'Export PDF',
    exportHtml: 'Export HTML',
  },
  toasts: {
    exportedTo: 'Exported to {{path}}',
    exportFailed: 'Export failed: {{message}}',
    comparing: 'Comparing flows...',
    comparisonComplete: 'Comparison complete',
    comparisonFailed: 'Comparison failed: {{message}}',
    reimported: 'Flow re-imported and re-analyzed',
    reimportFailed: 'Re-import failed: {{message}}',
  },
  dropOverlay: 'Drop flow file to open',
}

export const findings = {
  search: {
    placeholder: 'Search findings...',
  },
  summary: {
    count_one: '{{count}} finding',
    count_other: '{{count}} findings',
    healthAria: 'Health score {{score}} of 100: {{label}}',
  },
  severity: {
    error: 'Error',
    warning: 'Warning',
    info: 'Info',
  },
  // Compact badge variants (space-constrained chips keep the pre-i18n look)
  badge: {
    error: 'Error',
    warning: 'Warn',
    info: 'Info',
  },
  empty: {
    noneTitle: 'No findings',
    noneDescription: "The analysis didn't detect any issues. Your flow looks good!",
    noMatch: 'No matching findings',
  },
  selection: {
    selectedCount_one: '{{count}} selected',
    selectedCount_other: '{{count}} selected',
    fixAll: 'Fix all',
    fixing: 'Fixing…',
    suppress: 'Suppress',
    clear: 'Clear selection',
    select: 'Select finding',
    deselect: 'Deselect finding',
    assignMe: 'Assign to me',
    resolve: 'Resolve',
  },
  toolbar: {
    toggleGrouping: 'Toggle duplicate grouping',
  },
  dedup: {
    groupedSummary: 'Grouped: {{unique}} unique findings ({{duplicates}} duplicates folded)',
    showAll: 'Show all',
  },
  suppressed: {
    summary_one: '{{count}} finding suppressed',
    summary_other: '{{count}} findings suppressed',
    restoreAll: 'Restore all',
  },
  progress: {
    analyzing: 'Analyzing… {{percent}}%',
    analyzingIndeterminate: 'Analyzing…',
    analysisLabel: 'Analysis progress',
    noRunYet: 'No analysis run yet',
    runAnalysis: 'Run Analysis',
  },
  toasts: {
    snapshotRestored: 'Snapshot restored',
  },
  chat: {
    scrollToBottom: 'Scroll to bottom',
  },
}

export const auth = {
  login: {
    titleSignIn: 'Sign in',
    titleRegister: 'Create Account',
    subtitleSignIn: 'Enter your credentials to continue',
    subtitleRegister: 'Join PAD Analyzer today',
    email: 'Email',
    password: 'Password',
    confirmPassword: 'Confirm Password',
    submitSignIn: 'Sign in',
    submitRegister: 'Sign up',
    hasAccount: 'Already have an account?',
    noAccount: "Don't have an account?",
    switchToSignIn: 'Sign in',
    switchToRegister: 'Sign up',
    passwordsDoNotMatch: 'Passwords do not match',
    resetPrompt: "Enter your account email and we'll send you a reset link",
    resetSent: 'If an account exists for that address, we’ve sent a reset link.',
  },
}

export const en = {common, shell, findings, auth}
