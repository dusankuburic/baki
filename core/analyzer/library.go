package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

// LibraryRule is a rule over a library of flows (ADR 0014, "Library rules").
// It runs in two phases so the library pass never re-reads a document:
// Extract condenses one flow into bounded JSON facts, CheckLibrary reads the
// facts of every flow. Library rules have their own registry: they are never
// in AllRules(), the per-flow walk, its reports and caches, baselines or gates.
type LibraryRule interface {
	Rule
	// FactsVersion changes whenever Extract's output changes meaning; facts
	// at another version are stale and never reach CheckLibrary.
	FactsVersion() int
	// Extract returns the rule's facts about ctx.Flow (JSON, at most
	// MaxLibraryFactsBytes), or nil when there is nothing to record.
	Extract(ctx *RuleContext) (json.RawMessage, error)
	// CheckLibrary reads the library, which shows the rule only its own facts.
	// The runner stamps ID, fingerprint, severity, confidence, category, stage.
	CheckLibrary(ctx context.Context, lib Library) ([]models.LibraryFinding, error)
}

// Library is a set of flow summaries. Each visits them in FlowID order and
// stops at the first error fn returns, returning it.
type Library interface {
	Len() int
	Each(fn func(models.FlowSummary) error) error
}

const (
	// MaxLibraryFactsBytes caps one rule's facts about one flow.
	MaxLibraryFactsBytes = 8 << 10
	// MaxLibraryFlows caps the flows one library pass reads (Truncated beyond).
	MaxLibraryFlows = 10_000

	maxLibraryFindingsPerRule = 10_000
	maxLibraryRelated         = 20
	libraryExtractWorkers     = 8
)

var registeredLibraryRules []LibraryRule

// registerLibraryRule adds r to the library registry and its metadata to the
// rule metadata table, so LookupMeta, MetaOf, IsPreview and RuleEnabled see
// it. Called from a library rule file's init only.
func registerLibraryRule(r LibraryRule, m RuleMeta) {
	t := metaTable()
	if t == nil {
		t = ruleMetaTable{}
		ruleMetas.Store(&t)
	}
	if _, dup := t[r.ID()]; dup {
		panic("analyzer: library rule " + r.ID() + " reuses a registered rule ID")
	}
	registeredLibraryRules = append(registeredLibraryRules, r)
	t[r.ID()] = &m
}

// LibraryRules returns every registered library rule, in registration order.
func LibraryRules() []LibraryRule {
	return slices.Clone(registeredLibraryRules)
}

// libraryRuleContext builds the RuleContext library rules extract from, with
// the per-flow engine's builder but outside its walk, reports and caches.
func libraryRuleContext(doc *models.FlowDocument, settings *models.AppSettings) *RuleContext {
	return buildContext(doc, settings)
}

// ExtractFlowSummary runs every enabled library rule's Extract over doc. A
// rule that fails, panics, returns invalid JSON or more than
// MaxLibraryFactsBytes is listed in Skipped and the others still run. The
// error is non-nil only for a nil doc or a done ctx (the summary is then
// incomplete). The RuleContext is built only when an enabled rule needs it.
func ExtractFlowSummary(ctx context.Context, doc *models.FlowDocument, settings *models.AppSettings,
	rules []LibraryRule) (models.FlowSummary, error) {
	if doc == nil {
		return models.FlowSummary{}, errors.New("analyzer: nil flow document")
	}
	if err := ctx.Err(); err != nil {
		return models.FlowSummary{}, err
	}
	sum := models.FlowSummary{FlowID: doc.ID, StableID: StableFlowID(doc), FlowName: doc.Name, ContentHash: FlowHash(doc)}
	var rctx *RuleContext
	for _, r := range rules {
		if !RuleEnabled(r, settings) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if rctx == nil {
			rctx = libraryRuleContext(doc, settings)
		}
		data, err := safeLibraryExtract(r, rctx)
		if err == nil && len(data) > MaxLibraryFactsBytes {
			err = fmt.Errorf("facts are %d bytes, over the %d-byte cap", len(data), MaxLibraryFactsBytes)
		}
		if err == nil && len(data) > 0 && !json.Valid(data) {
			err = errors.New("facts are not valid JSON")
		}
		if err != nil {
			logger.Warn("library rule extraction skipped", "rule", r.ID(), "flow", sum.FlowID, "error", err)
			sum.Skipped = append(sum.Skipped, r.ID())
			continue
		}
		if len(data) == 0 {
			continue
		}
		if sum.Facts == nil {
			sum.Facts = make(map[string]models.LibraryFacts, 1)
		}
		sum.Facts[r.ID()] = models.LibraryFacts{Version: r.FactsVersion(), Data: data}
	}
	return sum, nil
}

func safeLibraryExtract(r LibraryRule, ctx *RuleContext) (data json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			data, err = nil, fmt.Errorf("panic: %v", p)
		}
	}()
	return r.Extract(ctx)
}

// NewMemoryLibrary is a Library over summaries already in memory, sorted by
// FlowID (then StableID). The slice is copied.
func NewMemoryLibrary(summaries []models.FlowSummary) Library {
	l := slices.Clone(summaries)
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].FlowID != l[j].FlowID {
			return l[i].FlowID < l[j].FlowID
		}
		return l[i].StableID < l[j].StableID
	})
	return memoryLibrary(l)
}

type memoryLibrary []models.FlowSummary

func (l memoryLibrary) Len() int { return len(l) }

func (l memoryLibrary) Each(fn func(models.FlowSummary) error) error {
	for _, s := range l {
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

// AnalyzeLibrary extracts summaries of in-memory docs (up to 8 at a time) and
// runs the enabled library rules over them. It returns nil when no library
// rule is enabled under settings. A doc whose extraction fails is left out.
func AnalyzeLibrary(ctx context.Context, docs []*models.FlowDocument, rules []LibraryRule,
	settings *models.AppSettings) *models.LibraryReport {
	var enabled []LibraryRule
	for _, r := range rules {
		if RuleEnabled(r, settings) {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	summaries := make([]models.FlowSummary, len(docs))
	ok := make([]bool, len(docs))
	workers := min(runtime.GOMAXPROCS(0), libraryExtractWorkers, len(docs))
	idx := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				s, err := ExtractFlowSummary(ctx, docs[i], settings, enabled)
				summaries[i], ok[i] = s, err == nil
			}
		}()
	}
	for i := range docs {
		idx <- i
	}
	close(idx)
	wg.Wait()
	kept := summaries[:0]
	for i, s := range summaries {
		if ok[i] {
			kept = append(kept, s)
		}
	}
	return RunLibraryRules(ctx, enabled, NewMemoryLibrary(kept), settings)
}

// RunLibraryRules runs the enabled library rules (RuleEnabled) over lib and
// returns a deterministic report: findings in rule order, then by flow name,
// flow StableID, subflow name and fingerprint; numbered L-001 on. A rule that
// panics, fails or meets a done ctx is counted in RulesSkipped and contributes
// nothing. lib is read up to MaxLibraryFlows flows (Truncated beyond).
func RunLibraryRules(ctx context.Context, rules []LibraryRule, lib Library,
	settings *models.AppSettings) *models.LibraryReport {
	return runLibraryRules(ctx, rules, lib, settings, MaxLibraryFlows)
}

func runLibraryRules(ctx context.Context, rules []LibraryRule, lib Library, settings *models.AppSettings,
	maxFlows int) *models.LibraryReport {
	start := time.Now()
	report := &models.LibraryReport{Findings: []models.LibraryFinding{}}
	total := 0
	if lib != nil {
		total = lib.Len()
	}
	report.Flows = min(total, maxFlows)
	report.Truncated = total > maxFlows
	for _, r := range rules {
		if !RuleEnabled(r, settings) {
			continue
		}
		report.RulesRun++
		if ctx.Err() != nil {
			report.RulesSkipped++
			continue
		}
		view := &libraryView{lib: lib, rule: r, max: maxFlows, flows: make(map[string]libraryFlowRef)}
		found, err := safeCheckLibrary(ctx, r, view)
		if err == nil {
			err = ctx.Err() // cancelled mid-rule: the findings may be partial
		}
		if err != nil {
			logger.Warn("library rule skipped", "rule", r.ID(), "error", err)
			report.RulesSkipped++
			continue
		}
		if view.truncated {
			report.Truncated = true
		}
		found, capped := stampLibraryFindings(r, found, view.flows, settings)
		if capped {
			report.Truncated = true
		}
		report.Findings = append(report.Findings, found...)
	}
	for i := range report.Findings {
		report.Findings[i].ID = fmt.Sprintf("L-%03d", i+1)
	}
	report.GeneratedAt = time.Now()
	report.DurationMs = int(time.Since(start).Milliseconds())
	return report
}

func safeCheckLibrary(ctx context.Context, r LibraryRule, lib Library) (found []models.LibraryFinding, err error) {
	defer func() {
		if p := recover(); p != nil {
			logger.Error("library rule panicked; skipping", "rule", r.ID(), "panic", p)
			found, err = nil, fmt.Errorf("panic: %v", p)
		}
	}()
	return r.CheckLibrary(ctx, lib)
}

type libraryFlowRef struct{ stableID, name string }

// libraryView is the Library one rule sees: capped at max flows, showing only
// the rule's own facts at its FactsVersion, and recording which flows it
// streamed so findings can only name those.
type libraryView struct {
	lib       Library
	rule      LibraryRule
	max       int
	flows     map[string]libraryFlowRef
	truncated bool
}

var errLibraryCap = errors.New("library flow cap reached")

func (v *libraryView) Len() int {
	if v.lib == nil {
		return 0
	}
	return min(v.lib.Len(), v.max)
}

func (v *libraryView) Each(fn func(models.FlowSummary) error) error {
	if v.lib == nil {
		return nil
	}
	id, version := v.rule.ID(), v.rule.FactsVersion()
	n := 0
	err := v.lib.Each(func(s models.FlowSummary) error {
		if n == v.max {
			v.truncated = true
			return errLibraryCap
		}
		n++
		stable := s.StableID
		if stable == "" {
			stable = s.FlowID
		}
		v.flows[s.FlowID] = libraryFlowRef{stableID: stable, name: s.FlowName}
		facts, ok := s.Facts[id]
		s.Facts = nil
		if ok && facts.Version == version {
			s.Facts = map[string]models.LibraryFacts{id: facts}
		}
		return fn(s)
	})
	if errors.Is(err, errLibraryCap) {
		return nil
	}
	return err
}

// stampLibraryFindings fills what the runner owns, drops findings naming a
// flow the rule did not read, sorts, folds equal fingerprints (first wins) and
// caps the rule's findings (capped = some were dropped by the cap).
func stampLibraryFindings(r LibraryRule, found []models.LibraryFinding, flows map[string]libraryFlowRef,
	settings *models.AppSettings) (_ []models.LibraryFinding, capped bool) {
	meta := MetaOf(r)
	override := severityOverride(r, settings)
	type entry struct {
		stableID string
		f        models.LibraryFinding
	}
	entries := make([]entry, 0, len(found))
	unknown := 0
	for _, f := range found {
		flow, ok := flows[f.FlowID]
		if !ok {
			unknown++
			continue
		}
		f.RuleID = r.ID()
		if f.FlowName == "" {
			f.FlowName = flow.name
		}
		if f.Severity == "" {
			f.Severity = r.DefaultSeverity()
		}
		if override != "" {
			f.Severity = override
		}
		if f.Category == "" {
			f.Category = r.Category()
		}
		if f.Confidence == "" {
			f.Confidence = meta.Confidence
			if f.Confidence == "" {
				f.Confidence = models.ConfidenceMedium
			}
		}
		f.Stage = ""
		if meta.Stage == StagePreview {
			f.Stage = string(StagePreview)
		}
		f.Fingerprint = libraryFingerprint(r.ID(), flow.stableID, f.SubflowName, f.Anchor)
		f.Related = slices.Clone(f.Related)
		sort.SliceStable(f.Related, func(i, j int) bool {
			a, b := f.Related[i], f.Related[j]
			if a.FlowName != b.FlowName {
				return a.FlowName < b.FlowName
			}
			if a.SubflowName != b.SubflowName {
				return a.SubflowName < b.SubflowName
			}
			return a.FlowID < b.FlowID
		})
		if len(f.Related) > maxLibraryRelated {
			f.Related = f.Related[:maxLibraryRelated]
		}
		entries = append(entries, entry{stableID: flow.stableID, f: f})
	}
	if unknown > 0 {
		logger.Warn("library rule named flows it did not read; findings dropped", "rule", r.ID(), "count", unknown)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := &entries[i], &entries[j]
		if a.f.FlowName != b.f.FlowName {
			return a.f.FlowName < b.f.FlowName
		}
		if a.stableID != b.stableID {
			return a.stableID < b.stableID
		}
		if a.f.SubflowName != b.f.SubflowName {
			return a.f.SubflowName < b.f.SubflowName
		}
		return a.f.Fingerprint < b.f.Fingerprint
	})
	out := make([]models.LibraryFinding, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.f.Fingerprint] {
			continue
		}
		if len(out) == maxLibraryFindingsPerRule {
			return out, true
		}
		seen[e.f.Fingerprint] = true
		out = append(out, e.f)
	}
	return out, false
}

// libraryFingerprint is a library finding's stable identity: rule, the flow's
// StableID, subflow name and the rule's content anchor, never a parser-minted
// ID. Stored like Finding.Fingerprint: never change the formula.
func libraryFingerprint(ruleID, stableFlowID, subflowName, anchor string) string {
	h := sha256.New()
	for _, part := range [...]string{stableFlowID, subflowName, anchor} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return ruleID + ":" + hex.EncodeToString(h.Sum(nil)[:8])
}
