package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// The filterable analytics page (GET /api/dashboard/analytics). Findings are stored
// as per-flow rollups (a count per rule id), never per finding: every breakdown here
// — by severity, category, rule, fixability — is that rollup mapped through the
// org-aware rule catalog, which is what makes the filters possible over history
// without a schema change. The cost: a rule whose severity was overridden after a run
// is re-labelled by the current catalog — the payload says so (Applied.Derived), and
// the tiles use the stored totals whenever no finding filter is active.

const (
	maxAnalyticsFlows      = 5000
	analyticsActivityLimit = 50
	analyticsRuleCap       = 100
	analyticsFacetRuleCap  = 60
	analyticsFacetFlowCap  = 1000
	analyticsComplexityCap = 200
	analyticsWorstCap      = 10
	analyticsMoversCap     = 10
	analyticsSparkBuckets  = 12
	analyticsDay           = "2006-01-02"
	otherCategory          = "Other"
)

// AnalyticsQuery is the parsed, validated request (see api.parseAnalyticsQuery).
// From/To are midnights in Loc; the window is half-open [From, To).
type AnalyticsQuery struct {
	Preset   string
	From     time.Time
	To       time.Time
	Days     int
	Loc      *time.Location
	TZ       string
	Scope    string // personal | org | all
	OrgID    string
	Flows    []string
	Severity []string
	Category []string
	Rule     []string
	Compare  bool
}

// Key is the canonical cache key of the query (every parameter that changes
// the payload).
func (q AnalyticsQuery) Key() string {
	return strings.Join([]string{
		q.Preset, q.From.UTC().Format(time.RFC3339), q.To.UTC().Format(time.RFC3339), q.TZ,
		q.Scope, q.OrgID,
		strings.Join(q.Flows, ","), strings.Join(q.Severity, ","), strings.Join(q.Category, ","), strings.Join(q.Rule, ","),
		strconv.FormatBool(q.Compare),
	}, "|")
}

// PrevFrom is the start of the previous period of equal length.
func (q AnalyticsQuery) PrevFrom() time.Time { return q.From.AddDate(0, 0, -q.Days) }

// AnalyticsCaller carries what the payload needs to know about the caller beyond the
// user id: the email (failed-login lookup) and whether they may see instance-wide
// security counts.
type AnalyticsCaller struct {
	Email   string
	IsAdmin bool
}

// scopeResolver is what BuildAnalytics needs from the library service: the
// flow ids the caller may see under a scope and the org ids that widened it.
type scopeResolver interface {
	ScopeFlowIDs(ctx context.Context, userID, orgID string, scope LibraryScope, limit int) (ids []string, orgIDs []string, err error)
}

// anaCacheMaxEntries caps the analytics cache: the key embeds the caller-chosen
// window (From/To to the nanosecond) and flow list, so a client cycling query
// parameters can mint unlimited distinct keys, each holding a several-hundred-KB
// payload for the TTL — without a cap that is an unbounded, attacker-priced memory pool.
const anaCacheMaxEntries = 128

type anaCacheEntry struct {
	data      *models.DashboardAnalyticsData
	fetchedAt time.Time
	seq       uint64 // insertion order; breaks fetchedAt ties on eviction
}

// ruleMeta is the catalog view of one rule.
type ruleMeta struct {
	Name     string
	Category string
	Severity string
	AutoFix  bool
}

// CloudMode reports whether a durable backend is wired (analytics needs one).
func (s *DashboardService) CloudMode() bool { return s.backend != nil }

// SetScopeResolver wires the library service after construction (the DI
// adapter does it; tests inject a stub).
func (s *DashboardService) SetScopeResolver(r scopeResolver) { s.scope = r }

func (s *DashboardService) anaCacheGet(key string) *models.DashboardAnalyticsData {
	s.dashMu.Lock()
	defer s.dashMu.Unlock()
	if e, ok := s.anaCache[key]; ok && time.Since(e.fetchedAt) < dashboardCacheTTL {
		return e.data
	}
	return nil
}

func (s *DashboardService) anaCachePut(key string, data *models.DashboardAnalyticsData) {
	s.dashMu.Lock()
	defer s.dashMu.Unlock()
	if s.anaCache == nil {
		s.anaCache = make(map[string]*anaCacheEntry)
	}
	s.anaSeq++
	s.anaCache[key] = &anaCacheEntry{data: data, fetchedAt: time.Now(), seq: s.anaSeq}
	// Same write-time sweep as the home cache: entries past the TTL are dead
	// weight the hit check skips anyway.
	for k, e := range s.anaCache {
		if time.Since(e.fetchedAt) >= dashboardCacheTTL {
			delete(s.anaCache, k)
		}
	}
	// The TTL sweep cannot shrink a cache filled with fresh distinct keys, so also cap
	// the entry count: when over the cap, evict the entry closest to expiry (the
	// oldest fetchedAt — entries expire fetchedAt+TTL; equal timestamps fall back to
	// insertion order so the victim is deterministic). Same expired-first/nearest-expiry
	// order as pruneModelCacheLocked.
	for len(s.anaCache) > anaCacheMaxEntries {
		oldestKey, oldestAt, oldestSeq, first := "", time.Time{}, uint64(0), true
		for k, e := range s.anaCache {
			if first || e.fetchedAt.Before(oldestAt) || (e.fetchedAt.Equal(oldestAt) && e.seq < oldestSeq) {
				oldestKey, oldestAt, oldestSeq, first = k, e.fetchedAt, e.seq, false
			}
		}
		delete(s.anaCache, oldestKey)
	}
}

// BuildAnalytics assembles the analytics payload for userID under q. Scope errors
// (an org the caller is not a member of) propagate and are never cached; a failed
// section read is logged, named in Partial and left empty.
func (s *DashboardService) BuildAnalytics(ctx context.Context, userID string, caller AnalyticsCaller, q AnalyticsQuery) (*models.DashboardAnalyticsData, error) {
	if s.backend == nil {
		return nil, fmt.Errorf("analytics: cloud backend required")
	}
	if s.scope == nil {
		return nil, fmt.Errorf("analytics: no scope resolver wired")
	}
	key := userID + "\x00" + q.Key()
	if hit := s.anaCacheGet(key); hit != nil {
		return hit, nil
	}
	v, err, _ := s.anaSF.Do("analytics:"+key, func() (any, error) {
		if hit := s.anaCacheGet(key); hit != nil {
			return hit, nil
		}
		out, err := s.buildAnalytics(ctx, userID, caller, q)
		if err != nil {
			return nil, err
		}
		s.anaCachePut(key, out)
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*models.DashboardAnalyticsData), nil
}

// catalogFor merges the deployment catalog with every scoped org's catalog
// (org custom rules and severity overrides); the first definition wins.
func (s *DashboardService) catalogFor(ctx context.Context, orgIDs []string) map[string]ruleMeta {
	out := map[string]ruleMeta{}
	if s.analysis == nil {
		return out
	}
	add := func(rules []models.Rule) {
		for _, r := range rules {
			if _, seen := out[r.ID]; seen {
				continue
			}
			out[r.ID] = ruleMeta{Name: r.Name, Category: r.Category, Severity: string(r.DefaultSeverity), AutoFix: r.AutoFix != ""}
		}
	}
	for _, org := range orgIDs {
		if org != "" {
			add(s.analysis.GetRulesFor(ctx, org))
		}
	}
	add(s.analysis.GetRulesFor(ctx, ""))
	return out
}

func libraryScopeOf(scope string) LibraryScope {
	switch scope {
	case "personal":
		return ScopeMine
	default:
		return ScopeAll
	}
}

func (s *DashboardService) buildAnalytics(ctx context.Context, userID string, caller AnalyticsCaller, q AnalyticsQuery) (*models.DashboardAnalyticsData, error) {
	ids, orgIDs, err := s.scope.ScopeFlowIDs(ctx, userID, q.OrgID, libraryScopeOf(q.Scope), maxAnalyticsFlows)
	if err != nil {
		return nil, err
	}
	truncated := len(ids) >= maxAnalyticsFlows
	scopeIDs := ids
	applied := []string{}
	if len(q.Flows) > 0 {
		in := make(map[string]bool, len(ids))
		for _, id := range ids {
			in[id] = true
		}
		ids = ids[:0:0]
		for _, id := range q.Flows {
			if in[id] {
				ids = append(ids, id)
				applied = append(applied, id)
			}
		}
	}
	catalog := s.catalogFor(ctx, orgIDs)
	ff := newFindingFilter(q)

	w := storageif.AnalyticsWindow{FlowIDs: ids, From: q.From, To: q.To, TZ: q.TZ}
	if q.Compare {
		w.PrevFrom, w.PrevTo = q.PrevFrom(), q.From
	}
	facetWindow := storageif.AnalyticsWindow{FlowIDs: scopeIDs, From: q.From, To: q.To, TZ: q.TZ}
	usageMode := "user"
	switch q.Scope {
	case "org":
		usageMode = "org"
	case "all":
		if len(orgIDs) > 0 {
			usageMode = "both"
		}
	}

	var (
		snaps    []storageif.AnalysisSnapshotRow
		scopeSn  []storageif.AnalysisSnapshotRow
		hist     *storageif.AnalysisHistoryRows
		triage   *storageif.TriageRows
		usage    *storageif.UsageRows
		activity *storageif.ActivityRows
		security *storageif.SecurityRows
		errs     = map[string]error{}
		errMu    sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	run := func(name string, fn func() error) {
		g.Go(func() error {
			if err := fn(); err != nil {
				logger.Warn("analytics: section failed", "section", name, "userId", userID, "err", err)
				errMu.Lock()
				errs[name] = err
				errMu.Unlock()
			}
			return nil
		})
	}
	run("findings", func() (err error) { snaps, err = s.backend.LoadAnalysisSnapshots(gctx, ids); return })
	if len(applied) > 0 {
		run("facets", func() (err error) { scopeSn, err = s.backend.LoadAnalysisSnapshots(gctx, scopeIDs); return })
	}
	run("trend", func() (err error) { hist, err = s.backend.LoadAnalysisHistory(gctx, w); return })
	run("triage", func() (err error) { triage, err = s.backend.LoadTriageAnalytics(gctx, w); return })
	run("ai", func() (err error) {
		usage, err = s.backend.LoadUsageAnalytics(gctx, userID, orgIDs, usageMode, w)
		return
	})
	run("activity", func() (err error) {
		activity, err = s.backend.LoadActivityAnalytics(gctx, userID, facetWindow, analyticsActivityLimit)
		return
	})
	run("security", func() (err error) {
		security, err = s.backend.LoadSecurityAnalytics(gctx, userID, caller.Email, caller.IsAdmin)
		return
	})
	_ = g.Wait()
	if len(applied) == 0 {
		scopeSn = snaps
	}

	out := emptyAnalytics()
	out.Applied = models.AnalyticsApplied{
		Scope: q.Scope, OrgID: q.OrgID, Flows: applied,
		Severity: nonNil(q.Severity), Category: nonNil(q.Category), Rule: nonNil(q.Rule),
		Compare: q.Compare, TZ: q.TZ, Derived: ff.derived,
	}
	out.Range = models.AnalyticsRange{Preset: q.Preset, From: q.From.Format(analyticsDay), To: q.To.AddDate(0, 0, -1).Format(analyticsDay), Days: q.Days}
	if q.Compare {
		out.Range.PrevFrom = q.PrevFrom().Format(analyticsDay)
		out.Range.PrevTo = q.From.AddDate(0, 0, -1).Format(analyticsDay)
	}
	for _, name := range []string{"findings", "facets", "trend", "triage", "ai", "activity", "security"} {
		if errs[name] != nil {
			out.Partial = append(out.Partial, name)
		}
	}

	loc := q.Loc
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	todayInWindow := !today.Before(q.From) && today.Before(q.To)

	// --- the current state of every analyzed flow in scope ---
	states := map[string]*flowState{}
	analyzed := 0
	for i := range snaps {
		sn := &snaps[i]
		if !sn.Analyzed {
			continue
		}
		analyzed++
		states[sn.FlowID] = &flowState{health: sn.HealthScore, errors: sn.Errors, warnings: sn.Warnings, info: sn.Info, autoFix: sn.AutoFixable, total: sn.TotalFindings, byRule: sn.ByRule}
	}
	out.Summary.FlowsInScope = len(ids)
	out.Summary.Unanalyzed = len(ids) - analyzed

	// --- trend: carry-forward sweep over history ---
	var prevPts, curPts []models.AnalyticsTrendPoint
	var prevEnd, sweepEnd map[string]*flowState
	if hist != nil {
		start := q.From
		if q.Compare {
			start = q.PrevFrom()
		}
		prevPts, curPts, prevEnd, sweepEnd = sweepTrend(hist, start, q.From, q.To, ff, catalog)
	}
	nowAgg := aggregateStates(states, ff, catalog)
	if todayInWindow && len(curPts) > 0 {
		// The line ends where the tiles are: today's point is the live snapshot.
		last := &curPts[len(curPts)-1]
		if last.Date == today.Format(analyticsDay) {
			runs := last.Runs
			*last = nowAgg
			last.Date, last.Runs = today.Format(analyticsDay), runs
		}
	} else if !todayInWindow && sweepEnd != nil && errs["trend"] == nil {
		// A window that ended in the past reads "as of its last day".
		nowAgg = aggregateStates(sweepEnd, ff, catalog)
	}
	out.Trend.Points, out.Trend.PrevPoints = nonNilPoints(curPts), nonNilPoints(prevPts)
	if !q.Compare {
		out.Trend.PrevPoints = []models.AnalyticsTrendPoint{}
	}

	// --- summary tiles ---
	asOf := today
	if !todayInWindow {
		asOf = q.To.AddDate(0, 0, -1)
	}
	out.Summary.AsOf = asOf.Format(analyticsDay)
	var prevAgg *models.AnalyticsTrendPoint
	if q.Compare && prevEnd != nil {
		a := aggregateStates(prevEnd, ff, catalog)
		prevAgg = &a
	}
	healthOK := nowAgg.FlowCount > 0
	out.Summary.AvgHealth = statAsOf(healthOK, floatOfIntPtr(nowAgg.AvgHealth), prevAgg, func(p models.AnalyticsTrendPoint) (float64, bool) {
		if p.AvgHealth == nil {
			return 0, false
		}
		return float64(*p.AvgHealth), true
	}, curPts, sparkLast)
	pick := func(f func(models.AnalyticsTrendPoint) int) func(models.AnalyticsTrendPoint) (float64, bool) {
		return func(p models.AnalyticsTrendPoint) (float64, bool) { return float64(f(p)), p.FlowCount > 0 }
	}
	out.Summary.OpenFindings = statAsOf(healthOK, float64(nowAgg.Findings), prevAgg, pick(func(p models.AnalyticsTrendPoint) int { return p.Findings }), curPts, sparkLast)
	out.Summary.Errors = statAsOf(healthOK, float64(nowAgg.Errors), prevAgg, pick(func(p models.AnalyticsTrendPoint) int { return p.Errors }), curPts, sparkLast)
	out.Summary.Warnings = statAsOf(healthOK, float64(nowAgg.Warnings), prevAgg, pick(func(p models.AnalyticsTrendPoint) int { return p.Warnings }), curPts, sparkLast)
	out.Summary.Info = statAsOf(healthOK, float64(nowAgg.Info), prevAgg, pick(func(p models.AnalyticsTrendPoint) int { return p.Info }), curPts, sparkLast)
	out.Summary.AutoFixable = statAsOf(healthOK, float64(nowAgg.AutoFixable), prevAgg, pick(func(p models.AnalyticsTrendPoint) int { return p.AutoFixable }), curPts, sparkLast)
	out.Summary.FlowsAnalyzed = statAsOf(true, float64(nowAgg.FlowCount), prevAgg, func(p models.AnalyticsTrendPoint) (float64, bool) { return float64(p.FlowCount), true }, curPts, sparkLast)
	runsOf := func(p models.AnalyticsTrendPoint) (float64, bool) { return float64(p.Runs), true }
	out.Summary.AnalysesRun = models.StatValue{Available: errs["trend"] == nil, Value: sumPoints(curPts, runsOf), Spark: sparkline(curPts, runsOf, sparkSum)}
	if q.Compare && errs["trend"] == nil {
		v := sumPoints(prevPts, runsOf)
		out.Summary.AnalysesRun.Prev = &v
	}

	// --- findings breakdown (latest analysis per flow) ---
	out.Findings = s.findingsSection(snaps, ff, catalog, nowAgg)
	out.Facets = facets(scopeSn, snaps, ff, catalog, truncated)

	// --- flows ---
	out.Flows = flowsSection(snaps, states, prevEnd, ff, catalog, q.Compare)

	// --- triage ---
	if triage != nil {
		out.Triage = triageSection(triage, ff, catalog, q)
		resolvedOf := func(d models.DateCount) (float64, bool) { return float64(d.Count), true }
		cur := out.Triage.ResolvedByDay
		out.Summary.Resolved = models.StatValue{Available: true, Value: sumDays(cur, resolvedOf), Spark: sparkDays(cur, resolvedOf, sparkSum)}
		if q.Compare {
			prev := dayGrid(q.PrevFrom(), q.From, resolvedPerDay(triage, ff, catalog))
			v := sumDays(prev, resolvedOf)
			out.Summary.Resolved.Prev = &v
		}
	}

	// --- AI usage ---
	if usage != nil {
		out.AI = aiSection(usage, q, usageMode, orgIDs)
		costOf := func(d models.AIDailyPoint) (float64, bool) { return d.Cost, true }
		spark := make([]float64, 0, analyticsSparkBuckets)
		for _, b := range buckets(len(out.AI.Daily)) {
			total := 0.0
			for _, p := range out.AI.Daily[b[0]:b[1]] {
				v, _ := costOf(p)
				total += v
			}
			spark = append(spark, round2(total))
		}
		out.Summary.AISpend = models.StatValue{Available: true, Value: out.AI.TotalCost, Prev: out.AI.PrevCost, Spark: spark}
	}

	// --- activity ---
	if activity != nil {
		out.Activity = activitySection(activity, userID, q)
	}

	// --- security ---
	out.Security = securitySection(security, snaps, catalog, caller, errs["security"] == nil)

	out.GeneratedAt = time.Now()
	return out, nil
}

// ---- filters and tallies ----

type findingFilter struct {
	sev     map[string]bool
	cat     map[string]bool
	rule    map[string]bool
	derived bool
}

func newFindingFilter(q AnalyticsQuery) *findingFilter {
	ff := &findingFilter{sev: toSet(q.Severity), cat: toSet(q.Category), rule: toSet(q.Rule)}
	ff.derived = len(ff.sev)+len(ff.cat)+len(ff.rule) > 0
	return ff
}

func (ff *findingFilter) without(dim string) *findingFilter {
	cp := &findingFilter{sev: ff.sev, cat: ff.cat, rule: ff.rule}
	switch dim {
	case "severity":
		cp.sev = nil
	case "category":
		cp.cat = nil
	case "rule":
		cp.rule = nil
	}
	cp.derived = len(cp.sev)+len(cp.cat)+len(cp.rule) > 0
	return cp
}

func categoryOf(meta ruleMeta, inCatalog bool) string {
	if !inCatalog || meta.Category == "" {
		return otherCategory
	}
	return meta.Category
}

func (ff *findingFilter) keep(id string, meta ruleMeta, inCatalog bool) bool {
	if len(ff.rule) > 0 && !ff.rule[id] {
		return false
	}
	if len(ff.sev) > 0 && !ff.sev[meta.Severity] {
		return false
	}
	if len(ff.cat) > 0 && !ff.cat[categoryOf(meta, inCatalog)] {
		return false
	}
	return true
}

type tally struct{ total, errors, warnings, info, autoFix int }

// tally counts one flow's findings under the filter: without a finding filter the
// stored totals are returned untouched (exactly what the home dashboard shows); with
// one, the by_rule rollup is mapped through the catalog.
func (ff *findingFilter) tally(byRule map[string]int, raw tally, cat map[string]ruleMeta) tally {
	if !ff.derived {
		return raw
	}
	var t tally
	for id, n := range byRule {
		meta, ok := cat[id]
		if !ff.keep(id, meta, ok) {
			continue
		}
		t.total += n
		switch meta.Severity {
		case "error":
			t.errors += n
		case "warning":
			t.warnings += n
		case "info":
			t.info += n
		}
		if meta.AutoFix {
			t.autoFix += n
		}
	}
	return t
}

type flowState struct {
	health, errors, warnings, info, autoFix, total int
	byRule                                         map[string]int
}

func (st *flowState) raw() tally {
	return tally{total: st.total, errors: st.errors, warnings: st.warnings, info: st.info, autoFix: st.autoFix}
}

func stateOfRow(r storageif.AnalysisHistoryRow) *flowState {
	return &flowState{health: r.HealthScore, errors: r.Errors, warnings: r.Warnings, info: r.Info, autoFix: r.AutoFixable, total: r.TotalFindings, byRule: r.ByRule}
}

func aggregateStates(states map[string]*flowState, ff *findingFilter, cat map[string]ruleMeta) models.AnalyticsTrendPoint {
	var p models.AnalyticsTrendPoint
	sum := 0
	for _, st := range states {
		p.FlowCount++
		sum += st.health
		t := ff.tally(st.byRule, st.raw(), cat)
		p.Findings += t.total
		p.Errors += t.errors
		p.Warnings += t.warnings
		p.Info += t.info
		p.AutoFixable += t.autoFix
	}
	if p.FlowCount > 0 {
		avg := int(math.Round(float64(sum) / float64(p.FlowCount)))
		p.AvgHealth = &avg
	}
	return p
}

// sweepTrend walks every local day from start to end, applying each day's latest
// analysis per flow to a carried-forward state and aggregating the fleet after each
// day; it returns both periods' points and the state at the end of the previous
// period and at the end.
func sweepTrend(hist *storageif.AnalysisHistoryRows, start, from, end time.Time, ff *findingFilter, cat map[string]ruleMeta) (prev, cur []models.AnalyticsTrendPoint, prevEnd, final map[string]*flowState) {
	state := map[string]*flowState{}
	for _, r := range hist.Baseline {
		state[r.FlowID] = stateOfRow(r)
	}
	byDay := map[string][]storageif.AnalysisHistoryRow{}
	for _, r := range hist.InWindow {
		byDay[r.Day] = append(byDay[r.Day], r)
	}
	fromKey := from.Format(analyticsDay)
	captured := false
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		key := d.Format(analyticsDay)
		if !captured && key >= fromKey {
			prevEnd = cloneStates(state)
			captured = true
		}
		runs := 0
		for _, r := range byDay[key] {
			state[r.FlowID] = stateOfRow(r)
			runs += r.Runs
		}
		p := aggregateStates(state, ff, cat)
		p.Date, p.Runs = key, runs
		if key < fromKey {
			prev = append(prev, p)
		} else {
			cur = append(cur, p)
		}
	}
	if !captured {
		prevEnd = cloneStates(state)
	}
	return prev, cur, prevEnd, state
}

func cloneStates(in map[string]*flowState) map[string]*flowState {
	out := make(map[string]*flowState, len(in))
	for k, v := range in {
		cp := *v
		out[k] = &cp
	}
	return out
}

// ---- stats and sparklines ----

type sparkMode int

const (
	sparkLast sparkMode = iota
	sparkSum
)

func buckets(n int) [][2]int {
	if n <= 0 {
		return nil
	}
	size := int(math.Ceil(float64(n) / analyticsSparkBuckets))
	if size < 1 {
		size = 1
	}
	var out [][2]int
	for i := 0; i < n; i += size {
		j := i + size
		if j > n {
			j = n
		}
		out = append(out, [2]int{i, j})
	}
	return out
}

func sparkline(pts []models.AnalyticsTrendPoint, pick func(models.AnalyticsTrendPoint) (float64, bool), mode sparkMode) []float64 {
	out := []float64{}
	for _, b := range buckets(len(pts)) {
		total, last, have := 0.0, 0.0, false
		for _, p := range pts[b[0]:b[1]] {
			v, ok := pick(p)
			if !ok {
				continue
			}
			total += v
			last, have = v, true
		}
		if !have {
			continue
		}
		if mode == sparkSum {
			out = append(out, total)
		} else {
			out = append(out, last)
		}
	}
	return out
}

func sumPoints(pts []models.AnalyticsTrendPoint, pick func(models.AnalyticsTrendPoint) (float64, bool)) float64 {
	total := 0.0
	for _, p := range pts {
		if v, ok := pick(p); ok {
			total += v
		}
	}
	return total
}

func statAsOf(available bool, value float64, prevAgg *models.AnalyticsTrendPoint, pick func(models.AnalyticsTrendPoint) (float64, bool), pts []models.AnalyticsTrendPoint, mode sparkMode) models.StatValue {
	st := models.StatValue{Available: available, Value: value, Spark: sparkline(pts, pick, mode)}
	if prevAgg != nil {
		if v, ok := pick(*prevAgg); ok {
			st.Prev = &v
		}
	}
	return st
}

func floatOfIntPtr(p *int) float64 {
	if p == nil {
		return 0
	}
	return float64(*p)
}

func sumDays(days []models.DateCount, pick func(models.DateCount) (float64, bool)) float64 {
	total := 0.0
	for _, d := range days {
		if v, ok := pick(d); ok {
			total += v
		}
	}
	return total
}

func sparkDays(days []models.DateCount, pick func(models.DateCount) (float64, bool), mode sparkMode) []float64 {
	out := []float64{}
	for _, b := range buckets(len(days)) {
		total, last := 0.0, 0.0
		for _, d := range days[b[0]:b[1]] {
			v, _ := pick(d)
			total += v
			last = v
		}
		if mode == sparkSum {
			out = append(out, total)
		} else {
			out = append(out, last)
		}
	}
	return out
}

// dayGrid returns every local day in [from, to) as a gap-filled count series.
func dayGrid(from, to time.Time, counts map[string]int) []models.DateCount {
	out := []models.DateCount{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		key := d.Format(analyticsDay)
		out = append(out, models.DateCount{Date: key, Count: counts[key]})
	}
	return out
}

// ---- sections ----

func (s *DashboardService) findingsSection(snaps []storageif.AnalysisSnapshotRow, ff *findingFilter, cat map[string]ruleMeta, nowAgg models.AnalyticsTrendPoint) models.AnalyticsFindings {
	out := models.AnalyticsFindings{
		BySeverity:   map[string]int{"error": nowAgg.Errors, "warning": nowAgg.Warnings, "info": nowAgg.Info},
		ByCategory:   []models.CategoryCount{},
		ByRule:       []models.RuleCount{},
		ByConfidence: map[string]int{},
	}
	type ruleAgg struct{ count, flows int }
	rules := map[string]*ruleAgg{}
	cats := map[string]*models.CategoryCount{}
	for i := range snaps {
		sn := &snaps[i]
		if !sn.Analyzed {
			continue
		}
		for tier, n := range sn.ByConfidence {
			out.ByConfidence[tier] += n
		}
		for id, n := range sn.ByRule {
			meta, ok := cat[id]
			if !ff.keep(id, meta, ok) {
				continue
			}
			ra := rules[id]
			if ra == nil {
				ra = &ruleAgg{}
				rules[id] = ra
			}
			ra.count += n
			ra.flows++
			c := categoryOf(meta, ok)
			cc := cats[c]
			if cc == nil {
				cc = &models.CategoryCount{Category: c}
				cats[c] = cc
			}
			cc.Count += n
			switch meta.Severity {
			case "error":
				cc.Errors += n
			case "warning":
				cc.Warnings += n
			case "info":
				cc.Info += n
			}
		}
	}
	for id, ra := range rules {
		meta, ok := cat[id]
		name := meta.Name
		if name == "" {
			name = id
		}
		out.ByRule = append(out.ByRule, models.RuleCount{Rule: id, Name: name, Category: categoryOf(meta, ok), Severity: meta.Severity, AutoFixable: meta.AutoFix, InCatalog: ok, Count: ra.count, Flows: ra.flows})
	}
	sort.Slice(out.ByRule, func(i, j int) bool {
		if out.ByRule[i].Count != out.ByRule[j].Count {
			return out.ByRule[i].Count > out.ByRule[j].Count
		}
		return out.ByRule[i].Rule < out.ByRule[j].Rule
	})
	if len(out.ByRule) > analyticsRuleCap {
		out.ByRule = out.ByRule[:analyticsRuleCap]
	}
	for _, cc := range cats {
		out.ByCategory = append(out.ByCategory, *cc)
	}
	sort.Slice(out.ByCategory, func(i, j int) bool {
		if out.ByCategory[i].Count != out.ByCategory[j].Count {
			return out.ByCategory[i].Count > out.ByCategory[j].Count
		}
		return out.ByCategory[i].Category < out.ByCategory[j].Category
	})
	totalRules, fixableRules := 0, 0
	for _, meta := range cat {
		totalRules++
		if meta.AutoFix {
			fixableRules++
		}
	}
	out.Fixability = models.FixabilityStub{Available: nowAgg.AutoFixable, Total: nowAgg.Findings, AutoFixableRules: fixableRules, TotalRules: totalRules}
	return out
}

// facets counts each filter dimension under the other dimensions' filters, so
// a chip says what choosing it would leave. Flows lists the whole scope.
func facets(scopeSnaps, snaps []storageif.AnalysisSnapshotRow, ff *findingFilter, cat map[string]ruleMeta, truncated bool) models.AnalyticsFacets {
	out := models.AnalyticsFacets{Severity: []models.FacetCount{}, Category: []models.FacetCount{}, Rule: []models.FacetCount{}, Flows: []models.FacetFlow{}, FlowsTruncated: truncated}
	sevCount := map[string]int{}
	catCount := map[string]int{}
	ruleCount := map[string]int{}
	fSev, fCat, fRule := ff.without("severity"), ff.without("category"), ff.without("rule")
	for i := range snaps {
		sn := &snaps[i]
		if !sn.Analyzed {
			continue
		}
		for id, n := range sn.ByRule {
			meta, ok := cat[id]
			if fSev.keep(id, meta, ok) {
				sevCount[meta.Severity] += n
			}
			if fCat.keep(id, meta, ok) {
				catCount[categoryOf(meta, ok)] += n
			}
			if fRule.keep(id, meta, ok) {
				ruleCount[id] += n
			}
		}
	}
	for _, sev := range []string{"error", "warning", "info"} {
		out.Severity = append(out.Severity, models.FacetCount{Key: sev, Label: sev, Count: sevCount[sev]})
	}
	for c, n := range catCount {
		out.Category = append(out.Category, models.FacetCount{Key: c, Label: c, Count: n})
	}
	sort.Slice(out.Category, func(i, j int) bool {
		if out.Category[i].Count != out.Category[j].Count {
			return out.Category[i].Count > out.Category[j].Count
		}
		return out.Category[i].Key < out.Category[j].Key
	})
	for id, n := range ruleCount {
		label := cat[id].Name
		if label == "" {
			label = id
		}
		out.Rule = append(out.Rule, models.FacetCount{Key: id, Label: label, Count: n})
	}
	sort.Slice(out.Rule, func(i, j int) bool {
		if out.Rule[i].Count != out.Rule[j].Count {
			return out.Rule[i].Count > out.Rule[j].Count
		}
		return out.Rule[i].Key < out.Rule[j].Key
	})
	if len(out.Rule) > analyticsFacetRuleCap {
		out.Rule = out.Rule[:analyticsFacetRuleCap]
	}
	flows := make([]storageif.AnalysisSnapshotRow, len(scopeSnaps))
	copy(flows, scopeSnaps)
	sort.Slice(flows, func(i, j int) bool { return flows[i].UpdatedAt.After(flows[j].UpdatedAt) })
	if len(flows) > analyticsFacetFlowCap {
		flows = flows[:analyticsFacetFlowCap]
	}
	for i := range flows {
		sn := &flows[i]
		f := models.FacetFlow{ID: sn.FlowID, Name: sn.Name, OrgID: sn.OrgID}
		if sn.Analyzed {
			h := sn.HealthScore
			at := sn.AnalyzedAt
			f.Health, f.AnalyzedAt = &h, &at
			f.Findings = ff.tally(sn.ByRule, tally{total: sn.TotalFindings}, cat).total
		}
		out.Flows = append(out.Flows, f)
	}
	return out
}

func flowsSection(snaps []storageif.AnalysisSnapshotRow, states, prevEnd map[string]*flowState, ff *findingFilter, cat map[string]ruleMeta, compare bool) models.AnalyticsFlows {
	out := models.AnalyticsFlows{
		HealthBuckets: []models.HealthBucketStub{{Label: "0-20", Lo: 0, Hi: 20}, {Label: "20-40", Lo: 20, Hi: 40}, {Label: "40-60", Lo: 40, Hi: 60}, {Label: "60-80", Lo: 60, Hi: 80}, {Label: "80-100", Lo: 80, Hi: 100}},
		Complexity:    []models.FlowPoint{},
		Worst:         []models.FlowPoint{},
		Movers:        []models.FlowMover{},
	}
	points := make([]models.FlowPoint, 0, len(snaps))
	for i := range snaps {
		sn := &snaps[i]
		if !sn.Analyzed {
			continue
		}
		b := sn.HealthScore / 20
		if b > 4 {
			b = 4
		}
		if b < 0 {
			b = 0
		}
		out.HealthBuckets[b].Count++
		t := ff.tally(sn.ByRule, tally{total: sn.TotalFindings, errors: sn.Errors, warnings: sn.Warnings, info: sn.Info, autoFix: sn.AutoFixable}, cat)
		points = append(points, models.FlowPoint{FlowID: sn.FlowID, Name: sn.Name, OwnerID: sn.OwnerID, BlockCount: sn.BlockCount, Findings: t.total, Errors: t.errors, Warnings: t.warnings, Info: t.info, Health: sn.HealthScore, AnalyzedAt: sn.AnalyzedAt})
	}
	byUpdated := make([]models.FlowPoint, len(points))
	copy(byUpdated, points)
	sort.Slice(byUpdated, func(i, j int) bool { return byUpdated[i].AnalyzedAt.After(byUpdated[j].AnalyzedAt) })
	for _, p := range byUpdated {
		if p.BlockCount > 0 {
			out.Complexity = append(out.Complexity, p)
		}
		if len(out.Complexity) >= analyticsComplexityCap {
			break
		}
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].Health != points[j].Health {
			return points[i].Health < points[j].Health
		}
		if points[i].Findings != points[j].Findings {
			return points[i].Findings > points[j].Findings
		}
		return points[i].Name < points[j].Name
	})
	if len(points) > analyticsWorstCap {
		points = points[:analyticsWorstCap]
	}
	out.Worst = append(out.Worst, points...)
	if compare && prevEnd != nil {
		names := map[string]string{}
		for i := range snaps {
			names[snaps[i].FlowID] = snaps[i].Name
		}
		for id, now := range states {
			before, ok := prevEnd[id]
			if !ok {
				continue
			}
			from, to := before.health, now.health
			if from == to {
				continue
			}
			out.Movers = append(out.Movers, models.FlowMover{FlowID: id, Name: names[id], HealthFrom: &from, HealthTo: &to, Delta: to - from})
		}
		sort.Slice(out.Movers, func(i, j int) bool {
			ai, aj := abs(out.Movers[i].Delta), abs(out.Movers[j].Delta)
			if ai != aj {
				return ai > aj
			}
			return out.Movers[i].Name < out.Movers[j].Name
		})
		if len(out.Movers) > analyticsMoversCap {
			out.Movers = out.Movers[:analyticsMoversCap]
		}
	}
	return out
}

func resolvedPerDay(t *storageif.TriageRows, ff *findingFilter, cat map[string]ruleMeta) map[string]int {
	counts := map[string]int{}
	for _, r := range t.ResolvedByDay {
		meta, ok := cat[r.RuleID]
		if ff.derived && !ff.keep(r.RuleID, meta, ok) {
			continue
		}
		counts[r.Date] += r.Count
	}
	return counts
}

func triageSection(t *storageif.TriageRows, ff *findingFilter, cat map[string]ruleMeta, q AnalyticsQuery) models.AnalyticsTriage {
	out := models.AnalyticsTriage{Funnel: map[string]int{}, ResolvedByDay: []models.DateCount{}, MttrHours: t.MttrHours, ResolvedCount: t.ResolvedCount, StaleCount: t.StaleCount}
	total := 0
	for _, r := range t.ByStatusRule {
		meta, ok := cat[r.RuleID]
		if ff.derived && !ff.keep(r.RuleID, meta, ok) {
			continue
		}
		out.Funnel[r.Status] += r.Count
		total += r.Count
	}
	out.Backlog = out.Funnel["open"] + out.Funnel["acknowledged"] + out.Funnel["in_progress"]
	out.Available = total > 0 || t.ResolvedCount > 0
	out.ResolvedByDay = dayGrid(q.From, q.To, resolvedPerDay(t, ff, cat))
	return out
}

func aiSection(u *storageif.UsageRows, q AnalyticsQuery, mode string, orgIDs []string) models.AnalyticsAI {
	scoped := "user"
	switch mode {
	case "org":
		scoped = "org"
	case "both":
		if len(orgIDs) > 0 {
			scoped = "user+orgs"
		}
	}
	out := models.AnalyticsAI{Scoped: scoped, FlowScoped: false, Daily: []models.AIDailyPoint{}, ByProvider: []models.ProviderCostStub{}, ByModel: []models.ModelUsage{}}
	fromKey, toKey, prevKey := q.From.Format(analyticsDay), q.To.Format(analyticsDay), q.PrevFrom().Format(analyticsDay)
	byDay := map[string]storageif.UsageDailyRow{}
	prevCost := 0.0
	for _, r := range u.Daily {
		if r.Date >= fromKey && r.Date < toKey {
			byDay[r.Date] = r
			out.TotalCost += r.Cost
			out.TotalTokens += r.TokensIn + r.TokensOut
		} else if q.Compare && r.Date >= prevKey && r.Date < fromKey {
			prevCost += r.Cost
		}
	}
	for d := q.From; d.Before(q.To); d = d.AddDate(0, 0, 1) {
		key := d.Format(analyticsDay)
		r := byDay[key]
		out.Daily = append(out.Daily, models.AIDailyPoint{Date: key, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: round2(r.Cost), Calls: r.Calls})
	}
	out.TotalCost = round2(out.TotalCost)
	if q.Compare {
		p := round2(prevCost)
		out.PrevCost = &p
	}
	for _, p := range u.ByProvider {
		out.ByProvider = append(out.ByProvider, models.ProviderCostStub{Provider: p.Provider, Cost: round2(p.Cost), TokensIn: p.TokensIn, TokensOut: p.TokensOut})
	}
	for _, m := range u.ByModel {
		out.ByModel = append(out.ByModel, models.ModelUsage{Provider: m.Provider, Model: m.Model, ServedModel: m.ServedModel, TokensIn: m.TokensIn, TokensOut: m.TokensOut, Calls: m.Calls, Cost: round2(m.Cost)})
	}
	return out
}

func activitySection(a *storageif.ActivityRows, userID string, q AnalyticsQuery) models.AnalyticsActivity {
	out := models.AnalyticsActivity{Events: []models.ActivityEvent{}, ByDay: []models.DateCount{}, ByAction: []models.ActionCount{}}
	for _, e := range a.Events {
		out.Events = append(out.Events, models.ActivityEvent{
			Action:       e.Action,
			Actor:        models.ActivityActor{ID: e.UserID, Name: e.ActorName, IsMe: e.UserID != "" && e.UserID == userID},
			ResourceType: e.ResourceType,
			ResourceID:   e.ResourceID,
			Label:        e.Label,
			CreatedAt:    e.CreatedAt,
		})
	}
	counts := map[string]int{}
	for _, d := range a.ByDay {
		counts[d.Date] = d.Count
	}
	out.ByDay = dayGrid(q.From, q.To, counts)
	for _, c := range a.ByAction {
		out.ByAction = append(out.ByAction, models.ActionCount{Action: c.Action, Count: c.Count})
	}
	return out
}

func securitySection(sec *storageif.SecurityRows, snaps []storageif.AnalysisSnapshotRow, cat map[string]ruleMeta, caller AnalyticsCaller, ok bool) models.AnalyticsSecurity {
	out := models.AnalyticsSecurity{Available: ok && sec != nil}
	for i := range snaps {
		sn := &snaps[i]
		if !sn.Analyzed {
			continue
		}
		for id, n := range sn.ByRule {
			meta, in := cat[id]
			if in && strings.EqualFold(meta.Category, "security") {
				out.SecurityFindings += n
			}
			// Same rule-id heuristic as the home posture card.
			lid := strings.ToLower(id)
			if strings.Contains(lid, "credential") || strings.Contains(lid, "sensitive") || strings.Contains(lid, "injection") {
				out.CredentialFindings += n
			}
		}
	}
	if sec != nil {
		out.FailedLogins24h = sec.FailedLogins24h
		if caller.IsAdmin && sec.LockedRead {
			n := sec.LockedAccounts
			out.LockedAccounts = &n
		}
	}
	return out
}

// ---- small helpers ----

func emptyAnalytics() *models.DashboardAnalyticsData {
	return &models.DashboardAnalyticsData{
		Applied:  models.AnalyticsApplied{Flows: []string{}, Severity: []string{}, Category: []string{}, Rule: []string{}},
		Trend:    models.AnalyticsTrend{Points: []models.AnalyticsTrendPoint{}, PrevPoints: []models.AnalyticsTrendPoint{}},
		Findings: models.AnalyticsFindings{BySeverity: map[string]int{}, ByCategory: []models.CategoryCount{}, ByRule: []models.RuleCount{}, ByConfidence: map[string]int{}},
		Flows:    models.AnalyticsFlows{HealthBuckets: []models.HealthBucketStub{}, Complexity: []models.FlowPoint{}, Worst: []models.FlowPoint{}, Movers: []models.FlowMover{}},
		Triage:   models.AnalyticsTriage{Funnel: map[string]int{}, ResolvedByDay: []models.DateCount{}},
		AI:       models.AnalyticsAI{Scoped: "user", Daily: []models.AIDailyPoint{}, ByProvider: []models.ProviderCostStub{}, ByModel: []models.ModelUsage{}},
		Activity: models.AnalyticsActivity{Events: []models.ActivityEvent{}, ByDay: []models.DateCount{}, ByAction: []models.ActionCount{}},
		Facets:   models.AnalyticsFacets{Severity: []models.FacetCount{}, Category: []models.FacetCount{}, Rule: []models.FacetCount{}, Flows: []models.FacetFlow{}},
		Partial:  []string{},
	}
}

func toSet(in []string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, v := range in {
		out[v] = true
	}
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilPoints(in []models.AnalyticsTrendPoint) []models.AnalyticsTrendPoint {
	if in == nil {
		return []models.AnalyticsTrendPoint{}
	}
	return in
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
