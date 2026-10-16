package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"

	"pad-analyzer/internal/redisx"
)

// With more than one replica a flow's analyses land on whichever replica the balancer picks, and the report pair and
// the record claims of AnalysisService were each replica's own (ADR 0032, addendum): an analysis the other replica
// had already recorded was "fresh" again (a second trend row and a second analysis webhook), an export there answered
// 409 "run analysis first" or exported an older run, and its diff compared against no run or an older one. Both now
// live in Redis when the deployment can run more than one replica (AnalysisOptions.Shared), and fail open to the
// replica's own state.

// analysisClaimKey is the run whose dashboard record is claimed, per flow (recordKey: its tenant and stored id):
// {"v":1,"kind":"analysis.claim","id":"<run>"}. A claim is swapped in with SET ... GET, so of two replicas recording the
// same run one sees its own value come back and is not fresh; a newer run replaces the claim, so content that changes
// back is fresh again, as on one replica.
var analysisClaimKey = redisx.Declare(redisx.Spec{
	Name:    analysisClaimKind,
	Kind:    redisx.KindString,
	Pattern: "baki:analysis:claim:{tenant}:{flow}",
	TTL:     analysisClaimTTL,
	Version: 1,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/service",
})

// analysisPairKey is the flow's last two distinct runs, a hash: "cur" and "prev" are run identities (runIdentity),
// "cur_at" when cur was last recorded (unix ms, the recording replica's clock), "cur_body" and "prev_body" the
// reports, each the gzip of {"v":1,"kind":"analysis.pair","report":{...}}. The pair never moves back to a run recorded
// before cur_at, and a reader whose own current run was recorded later serves its own. A body is absent for a report
// over sharedReportCap and for a background (scanner) run: a sweep over the library must not write the library's
// reports into Redis. A reader whose own pair holds the run uses it; otherwise it decodes the
// body, and without one it falls back to its own pair, as before.
var analysisPairKey = redisx.Declare(redisx.Spec{
	Name:    analysisPairKind,
	Kind:    redisx.KindHash,
	Pattern: "baki:analysis:pair:{tenant}:{flow}",
	TTL:     analysisPairTTL,
	Version: 1,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/service",
})

const (
	analysisClaimKind = "analysis.claim"
	analysisPairKind  = "analysis.pair"
	// analysisClaimTTL bounds how long a run stays claimed without being recorded again: past it, re-analyzing the
	// unchanged flow records once more (one trend row), as after the claim LRU of one replica evicted it.
	analysisClaimTTL = 7 * 24 * time.Hour
	// analysisPairTTL bounds the shared pair: an interactive session's diff and export are within it; past it each
	// replica answers from its own pair, as before.
	analysisPairTTL = 24 * time.Hour
	// sharedAnalysisWait bounds one Redis call on the analysis path.
	sharedAnalysisWait = 500 * time.Millisecond
	// sharedAnalysisPause is how long a failed call keeps the analysis path off Redis: an unresponsive server must
	// not add the wait to every analysis.
	sharedAnalysisPause = 30 * time.Second
	// maxDecodedReport bounds what one shared body may inflate to.
	maxDecodedReport = 64 << 20
)

// sharedRuns keeps the reports this replica decoded from the shared pair (sharedAnalysis.decoded), by the flow's tenant
// and stored id and the run: a chat turn or an export reads the pair's identities and serves a run decoded before from
// here, instead of fetching and inflating its body each time, and concurrent readers of one run share one decode.
var sharedRuns = cache.Declare("analysis.shared-runs",
	cache.Keys(cache.KindTenant, cache.KindResource, cache.KindContent),
	cache.ReplicaLocal("decoded copies of the runs the shared report pair (analysis.pair) stores, keyed by the run's identity, so a copy never goes stale; another replica decodes its own on a miss"))

// sharedRunsBudget bounds the decoded copies by estimated bytes (analyzer.ReportCost), beside the report pair's own
// budget (analysisReportBudget): a third of it, the runs of the flows being exported or chatted about meanwhile.
const sharedRunsBudget = analysisReportBudget / 3

// errRunNotStored is a decode that found no usable body: nothing is kept, and the own pair answers.
var errRunNotStored = errors.New("the shared run's report is not stored")

// sharedReportCap is the largest compressed report the pair stores (a var so tests can lower it). Most reports
// compress to a few tens of KiB; one above the cap is not shared.
var sharedReportCap = 512 << 10

// sharedAnalysis is the Redis side of the report pair and the record claims.
type sharedAnalysis struct {
	client *redis.Client
	// pausedUntil (unix nanoseconds) is when the last failed call stops keeping the calls off Redis.
	pausedUntil atomic.Int64
	// runs are the reports decoded from the pair (decoded).
	runs *cache.Typed[*models.AnalysisReport]
}

// newSharedAnalysis is nil without a client: every method of a nil one is a no-op that leaves the answer to the
// replica's own state.
func newSharedAnalysis(client *redis.Client) *sharedAnalysis {
	if client == nil {
		return nil
	}
	return &sharedAnalysis{client: client, runs: cache.New[*models.AnalysisReport](sharedRuns, sharedRunsBudget)}
}

// usable reports whether a call may go to Redis now; false for a nil one.
func (sh *sharedAnalysis) usable() bool {
	return sh != nil && time.Now().UnixNano() >= sh.pausedUntil.Load()
}

// failed pauses the calls after err and logs it (rate limited, the error's class only).
func (sh *sharedAnalysis) failed(site, msg string, err error) {
	sh.pausedUntil.Store(time.Now().Add(sharedAnalysisPause).UnixNano())
	redisx.LimitedWarn(site, msg, err)
}

// runIdentity names a run across replicas: what sameRun compares (the content, and the findings' rules,
// fingerprints, severities and stages, in order) and the stats. "" for a report of an uncached path, which carries
// no content hash and is never shared.
func runIdentity(r *models.AnalysisReport) string {
	if r == nil || r.ContentHash == "" {
		return ""
	}
	h := sha256.New()
	field := func(v string) { fmt.Fprintf(h, "%d:%s;", len(v), v) }
	field(r.ContentHash)
	fmt.Fprintf(h, "findings:%d;", len(r.Findings))
	for i := range r.Findings {
		f := &r.Findings[i]
		field(f.RuleID)
		field(f.Fingerprint)
		field(string(f.Severity))
		field(f.Stage)
	}
	s := r.Stats
	fmt.Fprintf(h, "stats:%d,%d,%d,%d,%d,%d,%d", s.Errors, s.Warnings, s.Info, s.BlocksAnalyzed, s.RulesRun, s.RulesSkipped, s.Suppressed)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// claimValue is the claim payload of a run.
func claimValue(id string) string {
	b, _ := json.Marshal(struct {
		redisx.Envelope
		ID string `json:"id"`
	}{redisx.Envelope{V: 1, Kind: analysisClaimKind}, id})
	return string(b)
}

func sharedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), sharedAnalysisWait)
}

// claim swaps r in as flow's claimed run: fresh when the claim named another run or none. ok is false when Redis did
// not answer; the caller then keeps its own claim's answer.
func (sh *sharedAnalysis) claim(ctx context.Context, flow recordKey, r *models.AnalysisReport) (fresh, ok bool) {
	if !sh.usable() {
		return false, false
	}
	id := runIdentity(r)
	if id == "" {
		return false, false
	}
	ctx, cancel := sharedCtx(ctx)
	defer cancel()
	val := claimValue(id)
	old, err := sh.client.SetArgs(ctx, analysisClaimKey.Key(flow.tenant, flow.flow), val, redis.SetArgs{TTL: analysisClaimTTL, Get: true}).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return true, true
	case err != nil:
		sh.failed("analysis_claim", "analysis: record claim not shared; this replica's own claim decides", err)
		return false, false
	}
	return old != val, true
}

// release gives the claim on flow back if it still names r, so the next analysis of r on any replica records it.
func (sh *sharedAnalysis) release(ctx context.Context, flow recordKey, r *models.AnalysisReport) {
	if !sh.usable() {
		return
	}
	id := runIdentity(r)
	if id == "" {
		return
	}
	ctx, cancel := sharedCtx(ctx)
	defer cancel()
	if err := releaseScript.Run(ctx, sh.client, []string{analysisClaimKey.Key(flow.tenant, flow.flow)}, claimValue(id)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		sh.failed("analysis_claim", "analysis: record claim not released; it expires on its own", err)
	}
}

// pairTouchScript is the cheap first step of a record, before any encoding: the same run as cur moves cur_at on to
// ARGV[2] (never back) and answers 0, or 1 when ARGV[4] = "1" asks for a body the pair lacks; another run answers 2.
// ARGV[1] the run, ARGV[3] the TTL in milliseconds.
var pairTouchScript = redis.NewScript(`
redis.call("PEXPIRE", KEYS[1], ARGV[3])
if redis.call("HGET", KEYS[1], "cur") ~= ARGV[1] then return 2 end
if tonumber(ARGV[2]) > tonumber(redis.call("HGET", KEYS[1], "cur_at") or "0") then redis.call("HSET", KEYS[1], "cur_at", ARGV[2]) end
if ARGV[4] == "1" and redis.call("HEXISTS", KEYS[1], "cur_body") == 0 then return 1 end
return 0
`)

// pairShiftScript records ARGV[1], recorded at ARGV[4] (unix ms), as the flow's current run: the same run only gains a
// body it lacked (ARGV[2], "" for none) and a later cur_at; another moves the current run, body and all, to prev,
// unless it was recorded before the current one (a record that lost a race, or one made while this replica's sharing
// was paused): then nothing moves (-1). ARGV[3] is the TTL in milliseconds.
var pairShiftScript = redis.NewScript(`
local cur = redis.call("HGET", KEYS[1], "cur")
local at = tonumber(ARGV[4])
local curAt = tonumber(redis.call("HGET", KEYS[1], "cur_at") or "0")
if cur == ARGV[1] then
  if ARGV[2] ~= "" then redis.call("HSETNX", KEYS[1], "cur_body", ARGV[2]) end
  if at > curAt then redis.call("HSET", KEYS[1], "cur_at", ARGV[4]) end
  redis.call("PEXPIRE", KEYS[1], ARGV[3])
  return 0
end
if cur and at < curAt then return -1 end
redis.call("HDEL", KEYS[1], "prev", "prev_body")
if cur then
  redis.call("HSET", KEYS[1], "prev", cur)
  local body = redis.call("HGET", KEYS[1], "cur_body")
  if body then redis.call("HSET", KEYS[1], "prev_body", body) end
end
redis.call("HDEL", KEYS[1], "cur_body")
redis.call("HSET", KEYS[1], "cur", ARGV[1], "cur_at", ARGV[4])
if ARGV[2] ~= "" then redis.call("HSET", KEYS[1], "cur_body", ARGV[2]) end
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return 1
`)

// record makes r, recorded at at (unix ms), the flow's current run in the shared pair; withBody stores the report too
// (interactive runs). A repeat of the current run costs one round trip and no encoding.
func (sh *sharedAnalysis) record(ctx context.Context, flow recordKey, r *models.AnalysisReport, at int64, withBody bool) {
	if !sh.usable() {
		return
	}
	id := runIdentity(r)
	if id == "" {
		return
	}
	ctx, cancel := sharedCtx(ctx)
	defer cancel()
	key := analysisPairKey.Key(flow.tenant, flow.flow)
	ttl := strconv.FormatInt(analysisPairTTL.Milliseconds(), 10)
	wantBody := "0"
	if withBody {
		wantBody = "1"
	}
	same, err := pairTouchScript.Run(ctx, sh.client, []string{key}, id, at, ttl, wantBody).Int()
	if err != nil {
		sh.failed("analysis_pair", "analysis: report pair not shared; this replica's own pair answers", err)
		return
	}
	if same == 0 {
		return
	}
	body := ""
	if withBody {
		body = encodeSharedReport(r)
	}
	if err := pairShiftScript.Run(ctx, sh.client, []string{key}, id, body, ttl, at).Err(); err != nil && !errors.Is(err, redis.Nil) {
		sh.failed("analysis_pair", "analysis: report pair not shared; this replica's own pair answers", err)
	}
}

// run answers which (current or previous) run the deployment's pair holds for flow; ownCur and ownPrev are this
// replica's own pair, its current run recorded at ownAt (unix ms). ok is false when the own pair must answer: Redis did
// not, the flow has no shared pair (never shared, or past its TTL), the own current run is another, newer one (a run
// recorded while the sharing was paused, or one the pair refused as older), or the run's report is not stored (over the
// cap, a sweep's run) and this replica does not hold it. Otherwise the report is this replica's own when it holds that
// run, else the decoded body (kept, sharedRuns); nil with ok means the pair has no previous
// run. A pair that restarted after its TTL knows no previous run: the own one answers then if the own pair's current
// run is the shared one.
func (sh *sharedAnalysis) run(flow recordKey, previous bool, ownCur, ownPrev *models.AnalysisReport, ownAt int64) (report *models.AnalysisReport, ok bool) {
	if !sh.usable() {
		return nil, false
	}
	ctx, cancel := sharedCtx(context.Background())
	defer cancel()
	key := analysisPairKey.Key(flow.tenant, flow.flow)
	vals, err := sh.client.HMGet(ctx, key, "cur", "prev", "cur_at").Result()
	if err != nil {
		sh.failed("analysis_pair", "analysis: report pair not shared; this replica's own pair answers", err)
		return nil, false
	}
	cur, _ := vals[0].(string)
	if cur == "" {
		return nil, false
	}
	if curAt, _ := vals[2].(string); ownCur != nil && ownAt > atoi64(curAt) && runIdentity(ownCur) != cur {
		return nil, false
	}
	id, field := cur, "cur_body"
	if previous {
		id, _ = vals[1].(string)
		field = "prev_body"
		if id == "" {
			if ownCur != nil && runIdentity(ownCur) == cur {
				return ownPrev, true
			}
			return nil, true
		}
	}
	for _, own := range []*models.AnalysisReport{ownCur, ownPrev} {
		if own != nil && runIdentity(own) == id {
			return own, true
		}
	}
	r := sh.decoded(ctx, flow, field, id)
	return r, r != nil
}

// decoded is the stored report of run id (field of flow's pair): kept from an earlier read, else fetched and decoded
// once however many readers ask at the same time, and kept (sharedRuns, charged by its estimated bytes). nil when it is
// not stored, cannot be read or is not that run.
func (sh *sharedAnalysis) decoded(ctx context.Context, flow recordKey, field, id string) *models.AnalysisReport {
	k, _ := sharedRuns.Key(flow.part, cache.Resource(flow.flow), cache.Content(id))
	r, err := sh.runs.GetOrFill(ctx, k, func(ctx context.Context) (*models.AnalysisReport, int64, error) {
		raw, err := sh.client.HGet(ctx, analysisPairKey.Key(flow.tenant, flow.flow), field).Bytes()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				sh.failed("analysis_pair", "analysis: report pair not shared; this replica's own pair answers", err)
			}
			return nil, 0, errRunNotStored
		}
		r := decodeSharedReport(raw)
		if r == nil || runIdentity(r) != id {
			return nil, 0, errRunNotStored
		}
		return r, analyzer.ReportCost(r), nil
	}, nil, "")
	if err != nil {
		return nil
	}
	return r
}

// atoi64 is a stored decimal, 0 when absent or unreadable.
func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// sharedReportBody is a stored report: {"v":1,"kind":"analysis.pair","report":{...},"contentHash":...,"ruleSet":
// {...}}. The report's JSON leaves out its content hash and rule set, which the reader needs (the run's identity, and
// what an export or a record reads of the rules), so they travel beside it.
type sharedReportBody struct {
	redisx.Envelope
	Report      *models.AnalysisReport `json:"report"`
	ContentHash string                 `json:"contentHash"`
	RuleSet     *models.RuleSet        `json:"ruleSet,omitempty"`
}

// capWriter fails a write that would take it past max, so an oversized report stops encoding early.
type capWriter struct {
	buf bytes.Buffer
	max int
}

var errOverCap = errors.New("over the shared report cap")

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, errOverCap
	}
	return w.buf.Write(p)
}

// encodeSharedReport is r's stored body, "" when it is over sharedReportCap compressed.
func encodeSharedReport(r *models.AnalysisReport) string {
	// A report whose in-memory estimate is far beyond what could compress under the cap is not encoded at all.
	if analyzer.ReportCost(r) > maxDecodedReport {
		return ""
	}
	w := &capWriter{max: sharedReportCap}
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return ""
	}
	body := sharedReportBody{Envelope: redisx.Envelope{V: 1, Kind: analysisPairKind}, Report: r, ContentHash: r.ContentHash, RuleSet: r.RuleSet}
	if err := json.NewEncoder(zw).Encode(body); err != nil {
		return ""
	}
	if err := zw.Close(); err != nil {
		return ""
	}
	return w.buf.String()
}

// decodeSharedReport reads a stored body; nil for one this release cannot read (left in place for its TTL).
func decodeSharedReport(raw []byte) *models.AnalysisReport {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer func() { _ = zr.Close() }()
	payload, err := io.ReadAll(io.LimitReader(zr, maxDecodedReport+1))
	if err != nil || len(payload) > maxDecodedReport {
		return nil
	}
	var body sharedReportBody
	if redisx.Decode(analysisPairKind, 1, payload, &body) != redisx.ResultOK || body.Report == nil {
		return nil
	}
	body.Report.ContentHash, body.Report.RuleSet = body.ContentHash, body.RuleSet
	return body.Report
}
