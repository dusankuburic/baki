package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// scanRecordKey is a flow's last complete scan, shared by the replicas (ADR 0032, addendum): its scan key and the
// "new rules" note it made. The sweep's lease makes the replicas' sweeps take turns, not one per interval, so with a
// memory per process every replica re-analyzed what another had scanned since its own last sweep, and a replica that
// joined re-analyzed the whole library. A flow whose inputs are unchanged since any replica's complete scan is
// skipped. A miss costs a re-analysis and never an alert (the inbox row decides those), so the key fails open: every
// error falls back to the replica's own memory.
var scanRecordKey = redisx.Declare(redisx.Spec{
	Name:    scanRecordKind,
	Kind:    redisx.KindString,
	Pattern: "baki:scanner:scan:{flow}",
	TTL:     scanRecordTTL,
	Version: scanRecordVersion,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/scanner",
})

const (
	scanRecordKind    = "scanner.scan"
	scanRecordVersion = 1
	// scanRecordTTL outlives any sensible PAD_SCAN_INTERVAL; a flow no sweep reached for longer is analyzed again,
	// as after a restart.
	scanRecordTTL = 7 * 24 * time.Hour
	// sharedScanWait bounds one Redis call of the sweep: a slow Redis costs a re-analysis, not a stalled sweep.
	sharedScanWait = time.Second
	// sharedScanPause is how long a failed call keeps the sweep off Redis: a sweep makes a call per changed flow, and
	// an unresponsive server must not cost each of them the wait.
	sharedScanPause = 30 * time.Second
)

// scanRecord is the payload: {"v":1,"kind":"scanner.scan","key":"<hex>","note":{...}}.
type scanRecord struct {
	redisx.Envelope
	Key  string      `json:"key"`
	Note *recordNote `json:"note,omitempty"`
}

// recordNote is a noticeNote on the wire: the notice's identity and the flow's new rules.
type recordNote struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Scope         string   `json:"scope"`
	ScopeID       string   `json:"scopeId,omitempty"`
	RuleSetDigest string   `json:"ruleSetDigest"`
	Rules         []string `json:"rules"`
}

// sharedScan is a flow's record as read back.
type sharedScan struct {
	key  string
	note *noticeNote
}

// sharedScans is the Redis side of the scan memory.
type sharedScans struct {
	client *redis.Client
	// pausedUntil (unix nanoseconds) is when the last failed call stops keeping the sweep off Redis.
	pausedUntil atomic.Int64
}

// usable reports whether a call may go to Redis now.
func (sh *sharedScans) usable() bool {
	return sh != nil && time.Now().UnixNano() >= sh.pausedUntil.Load()
}

// failed pauses the calls after err and logs it (rate limited, the error's class only).
func (sh *sharedScans) failed(site, msg string, err error) {
	sh.pausedUntil.Store(time.Now().Add(sharedScanPause).UnixNano())
	redisx.LimitedWarn(site, msg, err)
}

// WithSharedScans shares each flow's last complete scan with the other replicas through client (nil: each replica
// remembers its own, as one replica does). The app passes a client only when the deployment can run more than one
// replica (PAD_MAX_REPLICAS > 1).
func WithSharedScans(client *redis.Client) Option {
	return func(s *Scanner) {
		if client != nil {
			s.shared = &sharedScans{client: client}
		}
	}
}

// load reads the records of a page's flows in one MGET; nil when Redis did not answer (the sweep then has its own
// memory only). A record this release cannot read is skipped and left for its TTL.
func (sh *sharedScans) load(ctx context.Context, ids []string) map[string]sharedScan {
	if !sh.usable() || len(ids) == 0 {
		return nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = scanRecordKey.Key(id)
	}
	ctx, cancel := context.WithTimeout(ctx, sharedScanWait)
	defer cancel()
	vals, err := sh.client.MGet(ctx, keys...).Result()
	if err != nil {
		sh.failed("scanner_shared_load", "scanner: shared scan records unreadable; this replica's own memory decides the skips", err)
		return nil
	}
	out := make(map[string]sharedScan, len(ids))
	for i, v := range vals {
		raw, ok := v.(string)
		if !ok {
			continue
		}
		var rec scanRecord
		if redisx.Decode(scanRecordKind, scanRecordVersion, []byte(raw), &rec) != redisx.ResultOK || rec.Key == "" {
			continue
		}
		out[ids[i]] = sharedScan{key: rec.Key, note: rec.Note.note()}
	}
	return out
}

// store records key and note as flowID's last complete scan.
func (sh *sharedScans) store(ctx context.Context, flowID, key string, note *noticeNote) {
	if !sh.usable() || key == "" {
		return
	}
	b, err := json.Marshal(scanRecord{Envelope: redisx.Envelope{V: scanRecordVersion, Kind: scanRecordKind}, Key: key, Note: wireNote(note)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedScanWait)
	defer cancel()
	if err := sh.client.Set(ctx, scanRecordKey.Key(flowID), b, scanRecordTTL).Err(); err != nil {
		sh.failed("scanner_shared_store", "scanner: shared scan record not stored; another replica analyzes the flow again", err)
	}
}

// forget drops flowID's record, so no replica skips the flow on it. Best effort: a record left behind makes another
// replica skip the flow with its note, which still raises the notice (as a kept one, stored by that sweep's last
// flush).
func (sh *sharedScans) forget(ctx context.Context, flowID string) {
	if !sh.usable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedScanWait)
	defer cancel()
	if err := sh.client.Del(ctx, scanRecordKey.Key(flowID)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		sh.failed("scanner_shared_forget", "scanner: shared scan record not dropped; it expires on its own", err)
	}
}

func wireNote(n *noticeNote) *recordNote {
	if n == nil {
		return nil
	}
	return &recordNote{ID: n.notice.ID, Kind: n.notice.Kind, Scope: n.notice.Scope, ScopeID: n.notice.ScopeID,
		RuleSetDigest: n.notice.RuleSetDigest, Rules: n.rules}
}

func (r *recordNote) note() *noticeNote {
	if r == nil || r.ID == "" {
		return nil
	}
	return &noticeNote{notice: storageif.RuleNotice{ID: r.ID, Kind: r.Kind, Scope: r.Scope, ScopeID: r.ScopeID,
		RuleSetDigest: r.RuleSetDigest}, rules: r.Rules}
}
