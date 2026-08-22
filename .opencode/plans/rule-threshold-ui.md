# Plan: Rule Threshold UI + Admin "Scan Now" Wiring

Status: approved, ready to execute. All facts below verified against source on 2026-08-14.

## Goal

Make the 4 analyzer rules' numeric thresholds (`maxDepth`, `maxBlocks` ×2, `minRepeats`) configurable from the Settings UI (currently CLI-only via bakicli), and wire the existing admin "scan now" endpoint into the Admin Dashboard.

## Verified current state

- Endpoints exist: `GET /api/analyze/rules`, `POST /api/analysis/rule/enabled`, `POST /api/analysis/rule/config` (body `{ruleId, config:{enabled, severity, options}}`) — routes_chi.go:193-196, handlers_analysis.go:242-304.
- Threshold-consuming rules (option key, built-in default, read site):
  - `deep-nesting` → `maxDepth`, default 6 — rule_deep_nesting.go:16-25
  - `wide-loop` → `maxBlocks`, default 20 — rule_wide_loop.go:22-27
  - `large-subflow` → `maxBlocks`, default 50 — rule_large_subflow.go:32
  - `duplicate-action` → `minRepeats`, default 3 — rule_duplicate_action.go:23-28
  - Same defaults mirrored in core/models/settings.go:259-289.
- Pattern to mirror: registry side-tables `ruleConfidence` / `ruleAutoFix` (core/analyzer/engine.go:30, :58) + helpers `RuleConfidence`/`RuleAutoFix` (engine.go:104-118).
- `GetRules` (internal/service/analysis.go:342-379) surfaces enabled + severity override only — must add Thresholds + Options.
- `UpdateRuleConfig` merge (internal/service/analysis.go:425-441) preserves `Options` when the client sends only `{enabled, severity}` — verify + cover with a test (severity-only save must NOT wipe thresholds).
- Frontend: `Rule` type at frontend/src/types/analysis.ts:258-269; `RuleConfig` at :271-275 (already has `options?`); RulesPanel.tsx (157 lines) has Switch + severity SegmentedControl, no threshold inputs; shared `NumberField` component exists (frontend/src/components/shared/NumberField.tsx, commit-on-blur semantics).
- Cache: rule options are folded into the analysis cache key (core/analyzer/cache.go:230-249) — threshold changes invalidate cache automatically; NO cache work needed.
- Admin scan endpoint: `handleScannerScan` (internal/api/handlers_admin.go:295-312), admin-only, returns `{started: true}`, 503 when `PAD_SCAN_INTERVAL` unset. **Route CONFIRMED: `POST /api/admin/scanner/scan`** (routes_chi.go:329 parent `/api/admin` + :338 leaf `/scanner/scan`).
- Frontend admin client (frontend/src/api/admin.ts:50+) has no `triggerScan`. AdminDashboard.tsx renders Users/DataMigration/AuditLog sections, no scanner section.
- **UpdateRuleConfig options-preserving merge is ALREADY TESTED**: `internal/service/ruleconfig_test.go` `TestRuleConfig_ToggleDoesNotWipeOverrides` covers both the config-save path (Options nil → preserved) and the dedicated `SetRuleEnabled` endpoint, asserting severity + `maxDepth: 3` survive a toggle-style save. → Step 4 needs ONLY the new drift-guard test; no merge test required.

## Part 1 — Backend

### 1. core/models/analysis.go — extend Rule (after AutoFix field, line ~154)

```go
	// Thresholds declares the numeric options this rule consumes, so the UI can
	// render inputs without hardcoding rule knowledge. Empty for rules with no
	// configurable thresholds.
	Thresholds []RuleThreshold `json:"thresholds,omitempty"`
	// Options is the currently configured option values (e.g. {"maxDepth": 8}).
	// Nil/empty means every threshold sits at its built-in default.
	Options map[string]any `json:"options,omitempty"`
}

// RuleThreshold describes one numeric option a rule reads from its config
// (settings.Analysis.Rules[ruleID].Options[key]) — the metadata the settings UI
// needs to render and validate an input.
type RuleThreshold struct {
	Key     string `json:"key"`     // option key, e.g. "maxDepth"
	Label   string `json:"label"`   // human label, e.g. "Max nesting depth"
	Default int    `json:"default"` // built-in value when unset
	Min     int    `json:"min"`     // smallest accepted value (UI clamp floor)
}
```

### 2. core/analyzer/thresholds.go — new file (declarative side-table)

```go
package analyzer

import "pad-core/models"

// ruleThresholds declares the numeric options each rule consumes... (mirror the
// ruleConfidence/ruleAutoFix doc style). Keys/defaults MUST match what the rule
// actually reads (guarded by TestRuleThresholds_MatchRuleReads).
var ruleThresholds = map[string][]models.RuleThreshold{
	"deep-nesting":     {{Key: "maxDepth", Label: "Max nesting depth", Default: 6, Min: 1}},
	"wide-loop":        {{Key: "maxBlocks", Label: "Max blocks in loop body", Default: 20, Min: 1}},
	"large-subflow":    {{Key: "maxBlocks", Label: "Max blocks in subflow", Default: 50, Min: 1}},
	"duplicate-action": {{Key: "minRepeats", Label: "Min repeats to flag", Default: 3, Min: 2}},
}

// RuleThresholds returns the declared thresholds for a rule (nil when none).
func RuleThresholds(id string) []models.RuleThreshold { return ruleThresholds[id] }
```

### 3. internal/service/analysis.go — GetRules (lines 356-376)

- In the struct literal: `Thresholds: analyzer.RuleThresholds(r.ID()),`
- Inside the settings-override block (`if rc, ok := ...`), after Enabled/severity: `result[i].Options = rc.Options`

### 4. Tests (core/analyzer)

- `TestRuleThresholds_MatchRuleReads` (new, core/analyzer/thresholds_test.go) — complete draft:
```go
package analyzer

import (
	"reflect"
	"testing"

	"pad-core/models"
)

// Guards the declarative side-table against drift: the declared option keys and
// defaults MUST match what each rule actually reads (and settings.go defaults).
func TestRuleThresholds_MatchRuleReads(t *testing.T) {
	want := map[string][]models.RuleThreshold{
		"deep-nesting":     {{Key: "maxDepth", Label: "Max nesting depth", Default: 6, Min: 1}},
		"wide-loop":        {{Key: "maxBlocks", Label: "Max blocks in loop body", Default: 20, Min: 1}},
		"large-subflow":    {{Key: "maxBlocks", Label: "Max blocks in subflow", Default: 50, Min: 1}},
		"duplicate-action": {{Key: "minRepeats", Label: "Min repeats to flag", Default: 3, Min: 2}},
	}
	if !reflect.DeepEqual(ruleThresholds, want) {
		t.Errorf("ruleThresholds drifted from the rules' actual reads:\n got %+v\nwant %+v", ruleThresholds, want)
	}
	// Every declared rule must exist in the live catalog.
	catalog := map[string]bool{}
	for _, r := range AllRules() {
		catalog[r.ID()] = true
	}
	for id, ts := range ruleThresholds {
		if !catalog[id] {
			t.Errorf("thresholds declared for unknown rule %q", id)
		}
		for _, th := range ts {
			if th.Key == "" || th.Label == "" || th.Default < 1 || th.Min < 1 {
				t.Errorf("invalid threshold declaration %+v for %s", th, id)
			}
		}
	}
}
```
- ~~UpdateRuleConfig merge test~~ — NOT needed: `TestRuleConfig_ToggleDoesNotWipeOverrides` (internal/service/ruleconfig_test.go) already covers the options-preserving merge on both save paths.

## Part 2 — Frontend

### 5. frontend/src/types/analysis.ts

```ts
export interface RuleThreshold {
  key: string
  label: string
  default: number
  min: number
}
// extend Rule (line 258):
  thresholds?: RuleThreshold[]
  options?: Record<string, unknown>
```

### 6. frontend/src/components/settings/RulesPanel.tsx (VERIFIED against NumberField props)

`NumberField` (frontend/src/components/shared/NumberField.tsx:4-13) takes `{value, onCommit, min?, max?, step?, fallback (REQUIRED), integer?, className?}` — **no label prop** (caller renders it), clamps to `[min,max]` internally, and only fires `onCommit` when the clamped value differs (built-in dedup). So the handler needs NO clamping of its own.

Handler (next to handleSeverityChange):
```tsx
import NumberField from '@/components/shared/NumberField'

const handleThresholdChange = async (ruleId: string, key: string, value: number) => {
  const rule = rules.find(r => r.id === ruleId)
  if (!rule) return
  const options = {...rule.options, [key]: value}
  try {
    await analysisApi.updateRuleConfig(ruleId, {
      enabled: rule.enabled,
      severity: rule.defaultSeverity,
      options,
    })
    setRules(rules.map(r => (r.id === ruleId ? {...r, options} : r)))
  } catch (err) {
    toast.error('Failed to update threshold: ' + (err as Error).message)
  }
}
```

Render (immediately after the severity `{rule.enabled && (...)}` block, inside the rule row):
```tsx
{rule.enabled && rule.thresholds?.map(t => (
  <div key={t.key} className="flex items-center gap-2">
    <span className="text-2xs font-bold uppercase text-text-tertiary">{t.label}:</span>
    <NumberField
      value={Number(rule.options?.[t.key] ?? t.default)}
      onCommit={v => handleThresholdChange(rule.id, t.key, v)}
      min={t.min}
      fallback={t.default}
      className="w-20"
    />
  </div>
))}
```

### 7. frontend/src/components/settings/RulesPanel.test.tsx (extend existing harness)

Existing harness: mocks `@/api` → `{analysisApi:{getRules,setRuleEnabled,updateRuleConfig}}`, wraps in `ToastProvider`, uses `fireEvent`/`waitFor`. Extend the `getRules` seed with:
```ts
thresholds: [{key: 'maxDepth', label: 'Max nesting depth', default: 6, min: 1}],
options: {maxDepth: 8},
```
plus a second rule WITHOUT thresholds. New cases:
- (a) `screen.findByDisplayValue('8')` — configured value shown, not default 6; second rule renders no `spinbutton` (`screen.queryAllByRole('spinbutton')` length matches threshold count only).
- (b) fire `blur` on the input after `fireEvent.change(input, {target:{value:'10'}})` → `updateRuleConfig` called with `('deep-nesting', {enabled:true, severity:'error', options:{maxDepth:10}})` (merged: current severity + option preserved).
- (c) typing below min (`0`) then blur → onCommit clamps to `min` (1) — assert call receives `maxDepth: 1`.
- (d) rejection → `findByText(/Failed to update threshold/)`.

## Part 3 — Admin "Scan Now"

### 8. Route (CONFIRMED — no lookup needed)

`POST /api/admin/scanner/scan` (routes_chi.go:338, inside the `/api/admin` group at :329).

### 9. frontend/src/api/admin.ts

```ts
triggerScan: (): Promise<{started: boolean}> => request('/api/admin/scanner/scan'),
```

### 10. ScannerSection component (new, self-contained — mirrors DataMigrationSection pattern)

AdminDashboard composes section components (`UserManagementSection`, `DataMigrationSection`, `AuditLogSection` at AdminDashboard.tsx:90-97) and uses the shared `Button` (`variant`/`size`/`icon` props, :71-80). A self-contained section keeps AdminDashboard untouched except one import + render, and gives the test a tiny surface (mock only `@/api/admin`).

`frontend/src/components/admin/ScannerSection.tsx` — complete draft:
```tsx
import {useState} from 'react'
import {Radar, Loader} from 'lucide-react'
import clsx from 'clsx'
import Button from '@/components/shared/Button'
import {adminApi} from '@/api/admin'

// Manual trigger for the continuous-governance scanner (cloud mode). The scan
// runs detached on the backend; this only starts it. 503 means scanning is not
// configured (PAD_SCAN_INTERVAL unset).
export function ScannerSection() {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ok: boolean; text: string} | null>(null)

  const start = async () => {
    setBusy(true)
    setMsg(null)
    try {
      await adminApi.triggerScan()
      setMsg({ok: true, text: 'Governance scan started — results appear in the alerts bell.'})
    } catch (err) {
      const text =
        err instanceof Error && /not configured/i.test(err.message)
          ? 'Scanner not configured — set PAD_SCAN_INTERVAL on the server.'
          : 'Failed to start scan: ' + (err instanceof Error ? err.message : 'unknown error')
      setMsg({ok: false, text})
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="bg-surface-1 border border-border-default rounded-xl p-4 flex items-center justify-between gap-4">
      <div>
        <h3 className="text-sm font-bold text-text-primary flex items-center gap-2">
          <Radar size={14} className="text-brand-500" /> Governance Scanner
        </h3>
        <p className="text-xs text-text-tertiary mt-1">
          Re-analyze every stored flow now for drift and health regressions.
        </p>
        {msg && (
          <p className={clsx('text-xs mt-2', msg.ok ? 'text-block-action' : 'text-semantic-error')}>{msg.text}</p>
        )}
      </div>
      <Button variant="ghost" size="sm" icon={busy ? Loader : Radar} onClick={start} disabled={busy}
        className={clsx(busy && '[&_svg]:animate-spin')}>
        {busy ? 'Scanning…' : 'Scan now'}
      </Button>
    </section>
  )
}
```
Wire-up in AdminDashboard.tsx: `import {ScannerSection} from './ScannerSection'` + render between DataMigrationSection and AuditLogSection.

`frontend/src/components/admin/ScannerSection.test.tsx` — mock `@/api/admin` (`triggerScan`); cases: success → started message + button label resets; 503-with-"not configured" message; generic error path; button disabled while busy.

### 11. Swagger (optional, drive-by)
If docs/swagger.json/yaml enumerate `/api/admin/*` routes, add `/api/admin/scanner/scan` alongside; skip if admin routes aren't documented there today (verify with rg — do not expand scope otherwise).

## Verification gate (AGENTS.md)

```
go build ./... && go test ./... && golangci-lint run && \
cd frontend && npx tsc --noEmit && npm test && npm run lint
```

Plus race tests for touched Go packages (internal/service, internal/api).

## Scope summary

~10 files, all small: 2 new Go files (thresholds.go + test), 3 touched Go files (models, service, admin route already exists), 4 new/changed frontend files (types, RulesPanel + test, ScannerSection + test) + admin.ts one-liner + one AdminDashboard import/render. No schema changes, no new endpoints. Future threshold rules surface in the UI automatically via the declarative registry.

## Execution status

Plan is COMPLETE and fully drafted — every code block above is final (verified against source: NumberField props, RulesPanel test harness, AdminDashboard section pattern, route registration, merge-test coverage). Blocked ONLY by the session's `edit → deny *` permission rule (plan-file writes excepted). First action on unlock: apply §1 (core/models/analysis.go), then straight through to the verification gate in one run.
