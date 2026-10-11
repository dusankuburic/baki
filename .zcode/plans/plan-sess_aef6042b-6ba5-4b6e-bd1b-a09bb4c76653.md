# Clear "bad comments" — eliminate suppressions by fixing root causes

The only comments left in the frontend are 46 directives (41 `eslint-disable react-hooks/*`, 4 `@ts-expect-error`, 1 `///` reference). Deep-dive verdict per site (rationale recovered from git HEAD since the working-tree copies were swept): **17 sites are safely removable**; 28 are engineering-correct suppressions that stay (details in the final report).

## Phase 1 — cytoscape-dagre typings (removes 4 `@ts-expect-error`)
Add `src/types/cytoscape-dagre.d.ts` (auto-included by tsconfig `"include": ["src"]`):
```ts
declare module 'cytoscape-dagre' {
  import type cytoscape from 'cytoscape'
  const ext: cytoscape.Ext
  export default ext
}
```
(`cytoscape.Ext` is the vendor-documented extension slot in cytoscape's bundled index.d.ts; `unknown` would not satisfy `cytoscape.use()`.) Delete the 4 `// @ts-expect-error` lines in RuleDependencyView.tsx, GraphView.tsx, useCytoscapeGraph.ts, VariableLineageGraph.tsx. No other changes — existing `as cytoscape.LayoutOptions` casts stay.

## Phase 2 — Dropdown refs suppression (removes 1)
`Dropdown.tsx:~154`: hoist the render-created wrapper `() => close(true)` into `const closeFromItem = useCallback(() => close(true), [close])` and pass that to `renderItems`. The `react-hooks/refs` warning fires on the render-time construction; a hook-created callback is recognized as safe. Behavior-identical (still only invoked from item onClick; focus restoration preserved). Delete the 3-line disable comment.

## Phase 3 — exhaustive-deps removals (6 sites, all verified behavior-identical)
1. `LinkedIdentitiesSection.tsx:34` — add `toastSuccess, t, refetch` to deps (all stable; the hash is stripped on first run so any re-run is a no-op). Drop disable.
2. `TriageCard.tsx:38` — delete the `funnel = data.funnel ?? {}` variable, inline `data.funnel?.[k]` in the memo factory. Drop disable.
3. `useCytoscapeGraph.ts:196` (mount effect) — add `setGraphZoom, selectSubflow` (stable zustand actions, zero re-runs). Drop disable.
4. `GraphView.tsx:86` (mount effect) — add `selectBlock, selectSubflow, setGraphZoom` (same). Drop disable.
5. `RuleDependencyView.tsx:155` — wrap `load` in `useCallback([], ...)` (reads only refs/setters/module fns), add it to deps. Drop disable.
6. `LoginForm.tsx:125` — `onSuccess` via latest-ref (`onSuccessRef` updated in its own effect), call `onSuccessRef.current()`; add stable `loginWithSSOTicket` to deps. Drop disable. Covered by LoginForm*.test.tsx (4 files).

## Phase 4 — set-state-in-effect removals (6 sites)
1. `ChatUsageMeter.tsx:78` — delete the `if (!isStreaming) { setLive(0) }` branch; render `isStreaming ? live : 0`.
2. `LiveToolTrail.tsx:18` — same derived-idle pattern; the component renders null when idle, so the reset branch goes, display derives.
3. `CommandPalette.tsx:67` — replace reset-on-open effect with the React "adjust state during render" pattern (prev-`isOpen` check + `setQuery('')`/`setActiveIndex(0)` during render); rAF focus stays in a small effect.
4. `FindingsTab.tsx:41` — debounce mirror: render-phase adjust when the store's `findingSearch` changes externally (prev-value ref), preserving the debounce write-back direction.
5. `FindingsTab.tsx:66` — diff/dedup reset: render-phase adjust keyed on `doc?.id`/`report?.generatedAt` identity change.
6. `FlowTree.tsx:146` — render-phase adjust resetting `focusedIndex` when `rows` identity changes. Verify keyboard nav via existing Sidebar tests + navigation e2e.

## Phase 5 — Verification
Targeted tests first (Dropdown, CommandPalette, FindingsTab, ChatUsageMeter, LiveToolTrail, LoginForm×4, SourceEditor, useCytoscapeGraph, RuleDependencyView, TriageCard/useDashboardStats), then the full gate: `tsc --noEmit`, `eslint --max-warnings 0`, vitest 3,287, production build, all 57 e2e.

## Stays as-is (28 directives — the suppression IS the correct tool)
- Mount-once/identity-keyed effects where adding deps would break: SourceEditor (re-fetch would clobber unsaved edits), GraphView:112 + useCytoscapeGraph:227 (content deps = dagre layout thrash per keystroke), GlobalSearchOverlay:145 (adding `query` defeats the debounce), LoginForm SSO hash parse (useState-initializer alternative has StrictMode double-init hazards).
- Intentional trigger deps: ChatInput:142 (`userMsgCount` is a reactivity trigger for a `getState()` snapshot), StreamingBubble:7 (keyed once-per-stream timestamp).
- Rule false-positives: HomeDashboard/useDashboardStats (ref-in-cleanup request invalidation — the rule's local-copy fix would pin stale ids and cancel nothing), useAsync:45 (dynamic `[...deps, nonce]` spread — React-blessed generic-hook shape).
- External-system sync / one-shot consume patterns (fetch-loading entry sets, pendingFocus focus-then-clear, DOM-token reads, store getState snapshots): 17 sites where restructuring changes timing or freshness semantics with no test coverage on several.
- `/// <reference types="vite/client" />` — functional, stays.

Final report will list the remaining directives grouped with one-line reasons (in the message, not as new code comments).