package scanner

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"pad-core/logger"
	"pad-core/models"

	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// "New rules" notices (ADR 0016). A finding whose rule is newer than its flow's baseline or health snapshot (a rule
// the release added or revised, an org toggle, a custom-rule edit, a promotion from preview) raises no drift alert,
// nor a regression unless the snapshot's score counted the rule (unscoredRules). The sweep notes those rules per
// scope and current rule set, and stores one notice per scope and rule set; the database's primary key decides which
// notice is new, so it is announced once across restarts and replicas.

// pendingNotice is the current sweep's part of one notice.
type pendingNotice struct {
	notice storageif.RuleNotice // identity only: ID, Kind, Scope, ScopeID, RuleSetDigest
	rules  map[string]struct{}
	flows  map[string]struct{}
	// unflushed lists the flows noted since the last stored flush: if storing fails, their scans are forgotten so a
	// later sweep notes them again.
	unflushed []string
	// kept: an unchanged (skipped) flow still raises the notice; stored: a flush of this sweep wrote it. The sweep's
	// last flush writes a kept notice no flush stored, so the retention sweep counts from this sweep.
	kept, stored bool
}

// noticeNote is one flow's part of a notice: the notice's identity and the flow's new rules with findings. A flow's
// last complete scan keeps its note, which the flow notes again while it is skipped.
type noticeNote struct {
	notice storageif.RuleNotice // identity only
	rules  []string
}

func (s *Scanner) resetNotices() {
	s.noticeMu.Lock()
	s.notices = nil
	s.noticeMu.Unlock()
}

// noticeScope is the scope whose members a flow's notice reaches: its organisation, else its owner; every flow of a
// deployment without accounts is the deployment's. ok is false for a flow nobody could see the notice of.
func (s *Scanner) noticeScope(lf *storageif.FlowDocument) (scope, scopeID string, ok bool) {
	switch {
	case s.singleTenant:
		return storageif.RuleNoticeScopeApp, "", true
	case lf.OrganizationID != "":
		return storageif.RuleNoticeScopeOrg, lf.OrganizationID, true
	case lf.OwnerID != "":
		return storageif.RuleNoticeScopeUser, lf.OwnerID, true
	}
	return "", "", false
}

// newRulesNote is the note a flow's new rules (those with findings) make for its scope's notice of the current rule
// set; nil for none, or for a flow nobody could see the notice of.
func (s *Scanner) newRulesNote(lf *storageif.FlowDocument, current *models.RuleSet, ruleIDs []string) *noticeNote {
	if len(ruleIDs) == 0 || current == nil || current.Digest == "" {
		return nil
	}
	scope, scopeID, ok := s.noticeScope(lf)
	if !ok {
		return nil
	}
	return &noticeNote{
		notice: storageif.RuleNotice{ID: storageif.RuleNoticeID(scope, scopeID, current.Digest),
			Kind: storageif.RuleNoticeKindNewRules, Scope: scope, ScopeID: scopeID, RuleSetDigest: current.Digest},
		rules: slices.Clone(ruleIDs),
	}
}

// addNote adds a flow's note (nil: none) to the sweep's notices. A scanned flow's note is stored by its page's flush;
// a skipped flow's (kept) only keeps the notice alive, stored once by the sweep's last flush unless a page stored it.
func (s *Scanner) addNote(flowID string, n *noticeNote, kept bool) {
	if n == nil {
		return
	}
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	if s.notices == nil {
		s.notices = make(map[string]*pendingNotice)
	}
	p := s.notices[n.notice.ID]
	if p == nil {
		p = &pendingNotice{notice: n.notice, rules: map[string]struct{}{}, flows: map[string]struct{}{}}
		s.notices[n.notice.ID] = p
	}
	for _, r := range n.rules {
		p.rules[r] = struct{}{}
	}
	p.flows[flowID] = struct{}{}
	if kept {
		p.kept = true
		return
	}
	p.unflushed = append(p.unflushed, flowID)
}

// flushNotices stores the notices the sweep changed since the last flush, in one statement, and announces the ones
// that are new; the sweep's last flush also stores the kept notices no flush stored. A failed store forgets the
// contributing flows' scans: the notes stay pending for the next flush, and a later sweep re-scans the flows should
// that one fail too.
func (s *Scanner) flushNotices(ctx context.Context, last bool) {
	s.noticeMu.Lock()
	var batch []*storageif.RuleNotice
	var flows []string
	for _, id := range slices.Sorted(maps.Keys(s.notices)) {
		p := s.notices[id]
		if len(p.unflushed) == 0 && (!last || !p.kept || p.stored) {
			continue
		}
		n := p.notice
		n.RuleIDs = slices.Sorted(maps.Keys(p.rules))
		n.FlowCount = len(p.flows)
		batch = append(batch, &n)
		flows = append(flows, p.unflushed...)
	}
	s.noticeMu.Unlock()
	if len(batch) == 0 {
		return
	}

	inserted, err := s.backend.UpsertRuleNotices(ctx, batch)
	if err != nil {
		logger.Warn("scanner: store new-rules notices failed", "notices", len(batch), "err", err)
		for _, id := range flows {
			s.setLastScanKey(id, "")
		}
		return
	}
	s.noticeMu.Lock()
	for _, n := range batch {
		if p := s.notices[n.ID]; p != nil {
			p.unflushed, p.stored = nil, true
		}
	}
	s.noticeMu.Unlock()
	for _, n := range batch {
		if slices.Contains(inserted, n.ID) {
			s.pushNoticeSSE(ctx, n)
		}
	}
}

// flushNoticesDetached is the sweep's last flush, on a context the end of the sweep's does not cancel, bounded like a
// page read.
func (s *Scanner) flushNoticesDetached(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pageReadTimeout)
	defer cancel()
	s.flushNotices(fctx, true)
}

// pushNoticeSSE pings the notice's audience (an org's members, the user) so the bell updates; a deployment without
// accounts has no addressable user, and its bell catches up on the next poll.
func (s *Scanner) pushNoticeSSE(ctx context.Context, n *storageif.RuleNotice) {
	if s.sse == nil {
		return
	}
	var to []string
	switch n.Scope {
	case storageif.RuleNoticeScopeOrg:
		to = s.orgRoster(ctx, n.ScopeID)
	case storageif.RuleNoticeScopeUser:
		to = []string{n.ScopeID}
	}
	rules := "rules report"
	if len(n.RuleIDs) == 1 {
		rules = "rule reports"
	}
	for _, uid := range to {
		s.sse.EmitTo(uid, "governance:alert", map[string]any{
			"flowId":   "",
			"type":     string(notify.EventNewRules),
			"title":    fmt.Sprintf("%d new %s findings", len(n.RuleIDs), rules),
			"severity": "warning",
			"scope":    n.Scope,
		})
	}
}

// resolvedRuleSet is a digest's entry in the sweep's cache.
type resolvedRuleSet struct {
	rs     *models.RuleSet // nil: no record carries the digest any more, which reads as legacy
	failed bool            // the read failed: a comparison with it cannot be made this sweep
}

// recordedRuleSet is the rule set a baseline or snapshot recorded, for comparing with the current one: nil (a
// legacy record) without a digest or when no record carries it; the set itself when the read carried it; the
// current set when the digests are equal; else resolved through RuleSetsByDigest. ok is false when that read failed:
// the flow is then not compared (no alert, no notice) and is scanned again by a later sweep.
func (s *Scanner) recordedRuleSet(ctx context.Context, page pageInputs, digest string, full, current *models.RuleSet) (rs *models.RuleSet, ok bool) {
	switch {
	case current == nil || digest == "":
		return nil, true
	case full != nil:
		return full, true
	case digest == current.Digest:
		return current, true
	}
	s.orgChMu.Lock()
	r, cached := s.ruleSets[digest]
	s.orgChMu.Unlock()
	if !cached {
		s.resolvePageRuleSets(ctx, page, digest, current.Digest)
		s.orgChMu.Lock()
		r = s.ruleSets[digest]
		s.orgChMu.Unlock()
	}
	return r.rs, !r.failed
}

// resolvePageRuleSets resolves, in one read, every recorded digest of the page the sweep has not resolved yet (want
// among them; the current set's digest needs none), so a page costs at most one RuleSetsByDigest. A failed read is
// cached too: the rest of the sweep does not retry it.
func (s *Scanner) resolvePageRuleSets(ctx context.Context, page pageInputs, want, current string) {
	s.orgChMu.Lock()
	need := map[string]struct{}{want: {}}
	for _, d := range page.recordedDigests() {
		if _, done := s.ruleSets[d]; !done && d != current {
			need[d] = struct{}{}
		}
	}
	s.orgChMu.Unlock()
	digests := slices.Sorted(maps.Keys(need))

	rctx, cancel := context.WithTimeout(ctx, pageReadTimeout)
	got, err := s.backend.RuleSetsByDigest(rctx, digests)
	cancel()
	if err != nil {
		logger.Warn("scanner: load recorded rule sets failed; their flows are compared by a later sweep", "digests", len(digests), "err", err)
	}
	s.orgChMu.Lock()
	defer s.orgChMu.Unlock()
	if s.ruleSets == nil {
		s.ruleSets = make(map[string]resolvedRuleSet, len(digests))
	}
	for _, d := range digests {
		s.ruleSets[d] = resolvedRuleSet{rs: got[d], failed: err != nil}
	}
}

// recordedDigests lists the rule-set digests the page's baselines and snapshots carry.
func (p pageInputs) recordedDigests() []string {
	var out []string
	for _, bl := range p.baselines {
		if bl.RuleSetDigest != "" {
			out = append(out, bl.RuleSetDigest)
		}
	}
	for _, h := range p.health {
		if h.RuleSetDigest != "" {
			out = append(out, h.RuleSetDigest)
		}
	}
	return out
}
