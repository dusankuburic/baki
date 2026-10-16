package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

const (
	defaultSpillMaxBytes = 10 * 1024 * 1024 // 10 MB

	// A replica spills to audit-spill-<instance>.jsonl; the release before wrote one fixed name, adopted like any
	// orphan. A replica that takes over another's file renames it to <file>.adopt-<claimant>-<unixnano> first.
	spillFilePrefix     = "audit-spill-"
	spillFileExt        = ".jsonl"
	legacySpillFileName = "audit-spill.jsonl"
	claimMarker         = ".jsonl.adopt-"
	drainScratchSuffix  = ".drain.tmp"
	claimingSuffix      = ".claiming"

	// defaultAdoptAfter is how long a spill file must go untouched before another replica may take it over (see
	// claimOrphans); emptyOrphanAge is how long an empty one lingers before it is deleted.
	defaultAdoptAfter = 2 * time.Minute
	emptyOrphanAge    = 24 * time.Hour

	maxInstanceNameLen = 64
)

// spillInstanceName is the instance id as a file-name component: [A-Za-z0-9._-] only (anything else becomes "_"),
// at most maxInstanceNameLen bytes, "unnamed" when empty.
func spillInstanceName(instance string) string {
	var sb strings.Builder
	for i := 0; i < len(instance) && sb.Len() < maxInstanceNameLen; i++ {
		c := instance[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "unnamed"
	}
	return sb.String()
}

// spillFileFor is the name of the spill file of the replica with this instance id.
func spillFileFor(instance string) string {
	return spillFilePrefix + spillInstanceName(instance) + spillFileExt
}

// depthGauge sums the depth of the spill files one recorder holds (its own and the claims it replays) into
// pad_audit_spill_depth.
type depthGauge struct {
	mu sync.Mutex
	by map[*spillStore]int
}

func (g *depthGauge) set(s *spillStore, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.by == nil {
		g.by = map[*spillStore]int{}
	}
	g.by[s] = n
	g.publish()
}

func (g *depthGauge) forget(s *spillStore) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.by, s)
	g.publish()
}

func (g *depthGauge) publish() {
	total := 0
	for _, n := range g.by {
		total += n
	}
	setAuditSpillDepth(total)
}

// spillStore is a bounded, on-disk FIFO of audit events that overflowed the in-memory pool. The reaper drains
// it back into the pool when capacity returns, so a transient DB/throughput hiccup doesn't drop events.
//
// The store is a single append-only JSON-lines file guarded by a mutex. Drain rewrites the file without the
// consumed prefix — O(n) per drain, but the file is size-capped (default 10 MB) and drains are throttled, so it
// is cheap relative to the DB write it precedes.
//
// Each replica owns one file, named after its instance id, so several replicas can share a directory without
// rewriting each other's file; the mutex only has to cover this process. A replica that is gone leaves its file
// behind, and a later replica takes it over (claimOrphans). Events still in the file at process exit remain on
// disk and drain on the next boot's Start WHEN the spill dir is on persistent storage; the default temp-dir
// location trades that cross-restart durability for zero-config (operators wanting it set PAD_AUDIT_SPILL_DIR to a
// mounted volume). Events are written at least once: a replay after a crash between the insert and the file
// rewrite is absorbed by the idempotent insert (ON CONFLICT (id) DO NOTHING).
type spillStore struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
	// depth is the count of events currently on the spill file, maintained under mu (cheap) so the
	// pad_audit_spill_depth gauge is accurate without a file read on every observation.
	depth int
	// gauge sums this store with its recorder's other files into the metric; nil: the store alone.
	gauge *depthGauge
	// warnedUndecodable: Drain logged the file's first undecodable line; later ones are only counted. Guarded by mu.
	warnedUndecodable bool
}

// setDepth records the file's event count and publishes the gauge. The caller holds s.mu.
func (s *spillStore) setDepth(n int) {
	s.depth = n
	if s.gauge == nil {
		setAuditSpillDepth(n)
		return
	}
	s.gauge.set(s, n)
}

// countEvents is the number of complete, non-empty lines in the file at path (what Drain would see).
func countEvents(data []byte) int {
	n := 0
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		if nl > 0 {
			n++
		}
		data = data[nl+1:]
	}
	return n
}

func newSpillStore(dir, instance string, maxBytes int64) (*spillStore, error) {
	if maxBytes <= 0 {
		maxBytes = defaultSpillMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, spillFileFor(instance))
	// Ensure the file exists so HasItems/Stat don't error before the first spill.
	if _, err := os.Stat(p); os.IsNotExist(err) {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	s := &spillStore{path: p, maxBytes: maxBytes, gauge: &depthGauge{}}
	// A file left by the previous boot counts from the start, not from the first drain.
	if data, err := os.ReadFile(p); err == nil { //nolint:gosec // G304: path is operator-configured, not user input
		s.mu.Lock()
		s.setDepth(countEvents(data))
		s.mu.Unlock()
	}
	return s, nil
}

// openAdopted opens a file this replica claimed (claimOrphans, ownClaims) as a store to replay and delete. gauge is
// the recorder's, nil for the store alone.
func openAdopted(path string, gauge *depthGauge, maxBytes int64) (*spillStore, error) {
	if maxBytes <= 0 {
		maxBytes = defaultSpillMaxBytes
	}
	data, err := os.ReadFile(path) // #nosec G304 -- a spill file this process renamed to its own claim name
	if err != nil {
		return nil, err
	}
	s := &spillStore{path: path, maxBytes: maxBytes, gauge: gauge}
	s.mu.Lock()
	s.setDepth(countEvents(data))
	s.mu.Unlock()
	return s, nil
}

// removeIfDrained deletes the file when no complete event is left in it (a torn last line, from a writer that died
// mid-append, is unrecoverable) and reports whether it did.
func (s *spillStore) removeIfDrained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path) // #nosec G304 -- a spill file of this process
	if err == nil && bytes.IndexByte(data, '\n') >= 0 {
		return false
	}
	if err == nil {
		if rmErr := os.Remove(s.path); rmErr != nil {
			return false
		}
	}
	if s.gauge != nil {
		s.gauge.forget(s)
	}
	return true
}

// Spill appends an event to the spill file. When the file is at capacity the event is NOT stored (the caller
// diverts it to the log fallback) and pad_audit_spill_dropped_total is bumped. Returns whether the event was
// stored.
func (s *spillStore) Spill(e *storageif.AuditEvent) bool {
	return s.spillFor(e, spillReasonFull)
}

// spillFor is Spill counting the event under reason (spillReasonFull, spillReasonUnreachable). The cap is what
// keeps a long database outage from filling the disk: past it every event is a counted drop.
func (s *spillStore) spillFor(e *storageif.AuditEvent, reason string) bool {
	line, err := json.Marshal(e)
	if err != nil {
		return false
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tornTail() {
		line = append([]byte{'\n'}, line...)
	}
	if info, statErr := os.Stat(s.path); statErr == nil {
		if info.Size()+int64(len(line)) > s.maxBytes {
			recordAuditSpillDropped()
			return false
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return false
	}
	s.setDepth(s.depth + 1)
	recordAuditSpilled(reason)
	return true
}

// tornTail reports whether the file ends inside a line: a writer died mid-append. An append must then start with a
// newline, or the new event joins the fragment on one line that never decodes and is lost with it. The caller holds
// s.mu.
func (s *spillStore) tornTail() bool {
	f, err := os.Open(s.path) // #nosec G304 -- a spill file of this process
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
}

// Drain removes and returns up to max events from the head of the spill file, oldest first. A line that does not
// decode (a torn append, a damaged file) is skipped and counted, logged once per file, so it never holds up the lines
// after it.
func (s *spillStore) Drain(max int) []*storageif.AuditEvent {
	if max <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var out []*storageif.AuditEvent
	var undecodable int
	var firstErr error
	remaining := data
	for len(out) < max {
		nl := bytes.IndexByte(remaining, '\n')
		if nl < 0 {
			break
		}
		line := remaining[:nl]
		remaining = remaining[nl+1:]
		if len(line) == 0 {
			continue
		}
		var e storageif.AuditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			undecodable++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, &e)
	}
	// Rewrite the file with the unconsumed tail ATOMICALLY (temp + rename): a plain WriteFile truncates first,
	// so a mid-write failure would lose the unconsumed tail. With rename semantics, any failure leaves the
	// ORIGINAL file (including the events counted in `out`) fully intact — we report nothing drained and let
	// the reaper retry, rather than risk replaying duplicates or corrupting the queue.
	tmp := s.path + drainScratchSuffix
	// #nosec G703 -- s.path is the operator-configured spill file
	// (PAD_AUDIT_SPILL_DIR, or a temp dir), fixed at boot. No request data
	// reaches it; the only appended component is this constant suffix.
	if err := os.WriteFile(tmp, remaining, 0o600); err != nil {
		logger.Warn("audit spill: drain rewrite failed; leaving spill file intact",
			"path", s.path, "err", err)
		return nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		logger.Warn("audit spill: drain rename failed; leaving spill file intact",
			"path", s.path, "err", err)
		_ = os.Remove(tmp)
		return nil
	}
	s.setDepth(countEvents(remaining)) // not depth-len(out): a corrupt line is skipped without being replayed
	// Counted once the rewrite took the lines out: a failed rewrite leaves them for the next drain to count.
	recordAuditSpillUndecodable(undecodable)
	if undecodable > 0 && !s.warnedUndecodable {
		s.warnedUndecodable = true
		// The error, not the line: a fragment of an event can hold personal data.
		logger.Warn("audit spill: skipped lines that do not decode (a torn append or a damaged file); later ones are only counted in pad_audit_spill_undecodable_total",
			"file", filepath.Base(s.path), "lines", undecodable, "err", firstErr)
	}
	return out
}

// HasItems reports whether the spill file holds any events.
func (s *spillStore) HasItems() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	return err == nil && info.Size() > 0
}

// reSpill re-appends already-drained events back to the spill file WITHOUT bumping pad_audit_spilled_total:
// these events were already counted when first spilled, and this is a replay retrial (the pool filled
// mid-batch), not a new overflow. Used by the reaper when it can't push a whole drained batch.
func (s *spillStore) reSpill(events []*storageif.AuditEvent) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	torn := s.tornTail()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is operator-configured, not user input
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if torn {
		_, _ = f.Write([]byte{'\n'})
	}
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		_, _ = f.Write(line)
	}
	s.setDepth(s.depth + len(events))
}

// parseSpillName classifies a file name in the spill directory: base is the spill file it is (or was, for a claim),
// owner and at the claimant and the time of a claim ("" and zero for a spill file proper). base is "" for anything
// that is no spill file.
func parseSpillName(name string) (base, owner string, at time.Time, claim bool) {
	if i := strings.Index(name, claimMarker); i >= 0 {
		base = name[:i+len(spillFileExt)]
		if base != legacySpillFileName && !strings.HasPrefix(base, spillFilePrefix) {
			return "", "", time.Time{}, false
		}
		rest := name[i+len(claimMarker):]
		j := strings.LastIndexByte(rest, '-')
		if j <= 0 || j == len(rest)-1 || strings.Trim(rest[j+1:], "0123456789") != "" {
			return "", "", time.Time{}, false
		}
		var nanos int64
		if _, err := fmt.Sscan(rest[j+1:], &nanos); err != nil {
			return "", "", time.Time{}, false
		}
		return base, rest[:j], time.Unix(0, nanos), true
	}
	if name == legacySpillFileName || (strings.HasPrefix(name, spillFilePrefix) && strings.HasSuffix(name, spillFileExt)) {
		return name, "", time.Time{}, false
	}
	return "", "", time.Time{}, false
}

// ownClaims lists the claims a previous run of this instance left in dir (it died while replaying them): they are
// ours to continue, whatever their age.
func ownClaims(dir, instance string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	me := spillInstanceName(instance)
	var out []string
	for _, e := range ents {
		if _, owner, _, claim := parseSpillName(e.Name()); claim && owner == me && e.Type().IsRegular() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// claimOrphans takes over the spill files in dir that no live replica is writing, by renaming each to a claim name
// of this instance (<file>.adopt-<instance>-<unixnano>), and returns the claims to replay. Rename is atomic and
// fails for all but one of several replicas racing for a file, so a file is replayed by one replica, and only that
// one, never read while its owner appends to it.
//
// Liveness has no lock file and no heartbeat: a file counts as abandoned when it holds events and has not been
// modified for olderThan. Every spill, drain and re-spill of a live replica rewrites its file, and its reaper
// drains it every 500 ms, so a live replica's non-empty file is touched far more often than that. The one live
// replica that can lose its file is one whose reaper is stalled for olderThan (its queue stays over half full);
// its events are then replayed by the claimant, and the idempotent insert absorbs the overlap. A file that is
// empty is no work: it is left alone (an idle live replica has one) until it is emptyOrphanAge old, then deleted.
// A claim counts from the later of its modification and the claim time in its name. Claims of this instance
// (ownClaims) and our own spill file are never taken.
func claimOrphans(dir, instance string, olderThan time.Duration) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	me := spillInstanceName(instance)
	now := time.Now()
	var claimed []string
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, strings.TrimSuffix(legacySpillFileName, spillFileExt)) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		src := filepath.Join(dir, name)
		if strings.HasSuffix(name, drainScratchSuffix) || strings.HasSuffix(name, claimingSuffix) {
			// The scratch file of a drain that died (the file it was to replace is intact), or the marker of a claim that died.
			if age >= olderThan {
				_ = os.Remove(src)
			}
			continue
		}
		base, owner, claimedAt, claim := parseSpillName(name)
		if claim && claimedAt.After(info.ModTime()) {
			// A rename keeps the modification time, so a fresh claim of an old file would look abandoned.
			age = now.Sub(claimedAt)
		}
		switch {
		case base == "", claim && owner == me, !claim && name == spillFileFor(me):
			continue
		case age < olderThan:
			continue
		case info.Size() == 0:
			if claim || age >= emptyOrphanAge {
				_ = os.Remove(src)
			}
			continue
		}
		dst := filepath.Join(dir, fmt.Sprintf("%s.adopt-%s-%d", base, me, time.Now().UnixNano()))
		if takeFile(src, dst) {
			claimed = append(claimed, dst)
		}
	}
	return claimed
}

// takeFile renames src to dst if this caller is the only one trying. rename(2) is exclusive on Linux (the loser gets
// ENOENT), but Windows renames through a handle opened by path first, so several racers can all succeed, one after
// the other, and each believes it owns the file; the marker created with O_EXCL (also exclusive on network file
// systems) makes the claim exclusive everywhere. A marker its claimant left behind by dying goes stale and is removed
// by the next scan.
func takeFile(src, dst string) bool {
	marker, err := os.OpenFile(src+claimingSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- a spill file name in the configured directory
	if err != nil {
		return false // another replica is claiming it
	}
	_ = marker.Close()
	defer func() { _ = os.Remove(src + claimingSuffix) }()
	if _, err := os.Stat(src); err != nil {
		return false // claimed (and renamed away) between the listing and now
	}
	return os.Rename(src, dst) == nil
}
