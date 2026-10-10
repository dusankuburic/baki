package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
)

// OrgRuleService owns an organisation's custom analyzer rules (R4) and the org settings their
// export reads. A rule change is announced as a change of the org's analysis configuration
// (SettingsChanged{OrgSettings}), so its cached rule profile drops and the next analysis
// re-resolves; without it the change waits out the resolver's TTL.
//
// Rules are written strictly and read tolerantly (ADR 0015, docs/custom-rules.md): SaveRule refuses what the
// strict check refuses, and every read (the analysis, the list, the export) goes through ReadOrgRule.
type OrgRuleService struct {
	rules    storageif.OrgCustomRuleStore
	settings storageif.AppSettingsStore
	// bus may be nil (tests): nothing is announced.
	bus *eventbus.Bus
}

func NewOrgRuleService(rules storageif.OrgCustomRuleStore, settings storageif.AppSettingsStore, bus *eventbus.Bus) *OrgRuleService {
	return &OrgRuleService{rules: rules, settings: settings, bus: bus}
}

// OrgRule is a stored org custom rule read the way the analysis reads it.
type OrgRule struct {
	Row *storageif.OrgCustomRule
	// Config is the stored config as decoded, its unknown fields included. When the stored JSON does not decode it
	// is the fields that do (analyzer.PartialCustomRule) under the row's rule id, so an edit or an old bundle's
	// toggle of the echo keeps them; a newer release's row gives only its id, kind and schemaVersion.
	Config analyzer.CustomRuleConfig
	// Rule is the compiled rule, nil when Problem is set.
	Rule *analyzer.CustomRule
	// Warnings are what the analysis tolerates: unknown fields, a missing id, a built-in rule's id.
	Warnings []*analyzer.CustomRuleError
	// Problem is why the analysis skips the rule: JSON that does not decode (INVALID_JSON), a kind or schemaVersion
	// this build cannot run, or a value that does not compile (a severity, pattern or autoFix).
	Problem *analyzer.CustomRuleError
	// undecoded: the stored JSON does not decode into this build's fields, so Config is not the stored config.
	undecoded bool
}

// Encoded is the rule as an export carries it: its write encoding (analyzer.EncodeCustomRule), or the stored JSON as
// stored when it does not decode into this build's fields (a newer release's rule leaves this build whole).
func (r OrgRule) Encoded() (json.RawMessage, error) {
	if !r.undecoded {
		return analyzer.EncodeCustomRule(r.Config)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, r.Row.Config); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TooNew reports whether a newer release wrote the rule: its kind or schemaVersion is one this build cannot run.
func (r OrgRule) TooNew() bool {
	if r.Problem == nil {
		return false
	}
	switch r.Problem.Reason {
	case analyzer.ReasonUnknownKind:
		return true
	case analyzer.ReasonUnsupportedVersion:
		return r.Config.SchemaVersion > analyzer.CustomRuleSchemaVersion
	}
	return false
}

// ReadOrgRule decodes and compiles a stored row tolerantly (analyzer.CustomRuleTolerant).
func ReadOrgRule(row *storageif.OrgCustomRule) OrgRule {
	r := OrgRule{Row: row}
	cfg, err := analyzer.DecodeCustomRule(row.Config)
	if err != nil {
		r.Config = analyzer.PartialCustomRule(row.Config)
		r.Config.ID = row.RuleID
		r.Problem = customRuleProblem(err, row.RuleID)
		r.undecoded = true
		if newer := newerReleaseRule(row); newer.TooNew() {
			r.Config, r.Problem = newer.Config, newer.Problem
		}
		return r
	}
	r.Config = cfg
	compiled, warnings, err := analyzer.CompileCustomRule(cfg, analyzer.CustomRuleTolerant)
	r.Warnings = warnings
	if err != nil {
		r.Problem = customRuleProblem(err, cfg.ID)
		return r
	}
	r.Rule = compiled
	return r
}

// newerReleaseRule reads only the kind and schemaVersion of a stored config whose fields do not decode: a later kind
// or schemaVersion may give a field another JSON type (ADR 0015), and such a row is still a newer release's rule, which
// must not read as INVALID_JSON (an edit would overwrite it, the export would refuse the org). Its Config carries the
// row's rule id, kind and schemaVersion; a kind or schemaVersion of another JSON type reads as unset.
func newerReleaseRule(row *storageif.OrgCustomRule) OrgRule {
	r := OrgRule{Row: row, Config: analyzer.CustomRuleConfig{ID: row.RuleID}}
	var head struct {
		Kind          json.RawMessage `json:"kind"`
		SchemaVersion json.RawMessage `json:"schemaVersion"`
	}
	if json.Unmarshal(row.Config, &head) != nil {
		return r
	}
	_ = json.Unmarshal(head.Kind, &r.Config.Kind)
	_ = json.Unmarshal(head.SchemaVersion, &r.Config.SchemaVersion)
	for _, p := range analyzer.CheckCustomRule(r.Config, analyzer.CustomRuleTolerant) {
		if p.Reason == analyzer.ReasonUnknownKind || p.Reason == analyzer.ReasonUnsupportedVersion {
			r.Problem = p
		}
	}
	return r
}

// customRuleProblem returns err's *analyzer.CustomRuleError; the core returns no other kind, but a stray error
// still says why the rule is skipped.
func customRuleProblem(err error, ruleID string) *analyzer.CustomRuleError {
	var ce *analyzer.CustomRuleError
	if errors.As(err, &ce) {
		return ce
	}
	return &analyzer.CustomRuleError{Index: -1, RuleID: ruleID, Reason: analyzer.ReasonInvalidJSON, Msg: err.Error(), Err: err}
}

// List returns the org's rules, each read as the analysis reads it; enabledOnly is what the analysis compiles.
func (s *OrgRuleService) List(ctx context.Context, orgID string, enabledOnly bool) ([]OrgRule, error) {
	rows, err := s.rules.ListOrgCustomRules(ctx, orgID, enabledOnly)
	if err != nil {
		return nil, err
	}
	out := make([]OrgRule, len(rows))
	for i, row := range rows {
		out[i] = ReadOrgRule(row)
	}
	return out, nil
}

// OrgRuleSave is one write of an org custom rule: a full config, or (Config nil) a toggle of a stored row's
// Enabled that leaves its config as stored, so a rule that no longer compiles can still be paused.
type OrgRuleSave struct {
	// RowID is the stored row's id. Empty creates the rule, or replaces the org's rule with Config.ID.
	RowID string
	// Config is the rule; nil toggles RowID's Enabled.
	Config *analyzer.CustomRuleConfig
	// Enabled defaults to true on a full save; a toggle needs it.
	Enabled *bool
}

var (
	errOrgRuleRenamed = apperr.Conflict("", "a saved rule's id cannot change; create a rule with the new id and delete this one")
	errOrgRuleToggle  = apperr.Invalid("", "pausing or resuming a rule needs its id and enabled")
	errOrgRuleMissing = apperr.NotFound("", "no such rule in this organisation")
)

// SaveRule validates and stores one of orgID's custom rules and returns it as stored:
//   - a full save checks Config strictly (analyzer.CustomRuleStrict); a built-in rule's id is accepted only when the
//     stored rule already has it, so a rule saved before ids were checked can still be edited;
//   - the stored config's fields this build does not know are kept (a newer release's additive fields);
//   - a rule's id never changes, and a rule a newer release wrote is not overwritten: both 409 CONFLICT;
//   - a toggle (Config nil) changes Enabled only.
//
// A config the strict check refuses is its *analyzer.CustomRuleError, which the API maps to the field.
func (s *OrgRuleService) SaveRule(ctx context.Context, orgID string, req OrgRuleSave) (OrgRule, error) {
	rows, err := s.rules.ListOrgCustomRules(ctx, orgID, false)
	if err != nil {
		return OrgRule{}, err
	}
	find := func(match func(*storageif.OrgCustomRule) bool) *storageif.OrgCustomRule {
		for _, row := range rows {
			if match(row) {
				return row
			}
		}
		return nil
	}
	if req.Config == nil {
		if req.RowID == "" || req.Enabled == nil {
			return OrgRule{}, errOrgRuleToggle
		}
		row := find(func(r *storageif.OrgCustomRule) bool { return r.ID == req.RowID })
		if row == nil {
			return OrgRule{}, errOrgRuleMissing
		}
		toggled := *row
		toggled.Enabled = *req.Enabled
		if err := s.save(ctx, &toggled); err != nil {
			return OrgRule{}, err
		}
		return ReadOrgRule(&toggled), nil
	}

	cfg := *req.Config
	var stored *OrgRule
	row := find(func(r *storageif.OrgCustomRule) bool { return req.RowID != "" && r.ID == req.RowID })
	if row != nil && strings.TrimSpace(cfg.ID) != "" && row.RuleID != cfg.ID { // a blank id is MISSING_ID below
		return OrgRule{}, errOrgRuleRenamed.WithDetails(map[string]any{"ruleId": row.RuleID})
	}
	if row == nil && cfg.ID != "" {
		row = find(func(r *storageif.OrgCustomRule) bool { return r.RuleID == cfg.ID })
	}
	if row != nil {
		r := ReadOrgRule(row)
		stored = &r
		if r.TooNew() {
			return OrgRule{}, apperr.Conflict("", fmt.Sprintf(
				"rule %q was saved by a newer version of Baki and cannot be edited here: %s", row.RuleID, r.Problem.Msg)).
				WithDetails(map[string]any{"ruleId": row.RuleID, "reason": r.Problem.Reason})
		}
	}
	for _, p := range analyzer.CheckCustomRule(cfg, analyzer.CustomRuleStrict) {
		if p.Warning || (p.Reason == analyzer.ReasonReservedID && stored != nil && stored.Row.RuleID == cfg.ID) {
			continue // stored under this id already: editing a rule saved before built-in ids were refused
		}
		return OrgRule{}, p
	}
	rowID := req.RowID
	if stored != nil {
		cfg = cfg.WithUnknownFieldsFrom(stored.Config)
		if rowID == "" {
			rowID = stored.Row.ID
		}
	}
	if rowID == "" {
		rowID = uuid.NewString()
	}
	raw, err := analyzer.EncodeCustomRule(cfg)
	if err != nil {
		return OrgRule{}, fmt.Errorf("encode rule: %w", err)
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	saved := &storageif.OrgCustomRule{ID: rowID, OrgID: orgID, RuleID: cfg.ID, Config: raw, Enabled: enabled}
	if err := s.save(ctx, saved); err != nil {
		return OrgRule{}, err
	}
	return ReadOrgRule(saved), nil
}

// save upserts rule and, once stored, announces the org's changed analysis configuration.
func (s *OrgRuleService) save(ctx context.Context, rule *storageif.OrgCustomRule) error {
	if err := s.rules.SaveOrgCustomRule(ctx, rule); err != nil {
		if errors.Is(err, storageif.ErrOrgCustomRuleRenamed) {
			return errOrgRuleRenamed.Wrap(err)
		}
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: rule.OrgID})
	return nil
}

// Delete removes one of orgID's rules and, once removed, announces the org's changed analysis configuration.
func (s *OrgRuleService) Delete(ctx context.Context, orgID, id string) error {
	if err := s.rules.DeleteOrgCustomRule(ctx, orgID, id); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: orgID})
	return nil
}

// OrgSettings returns the org's settings layer (its per-rule profile lives in Analysis.Rules).
func (s *OrgRuleService) OrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error) {
	return s.settings.LoadOrgSettings(ctx, orgID)
}

// compileOrgRules is the org custom-rule layer of a rule profile: rows are the org's enabled rules, read tolerantly.
// A rule the analysis cannot run is skipped (the rule list shows why); problems and tolerated warnings are logged
// once per stored content, not on every resolve.
func compileOrgRules(orgID string, rows []*storageif.OrgCustomRule) []analyzer.Rule {
	var out []analyzer.Rule
	for _, row := range rows {
		r := ReadOrgRule(row)
		if (r.Problem != nil || len(r.Warnings) > 0) && orgRulesLogged.first(orgID, row) {
			if p := r.Problem; p != nil {
				logger.Warn("rule profile: stored custom rule skipped",
					"orgId", orgID, "ruleId", row.RuleID, "reason", p.Reason, "field", p.Field, "error", p.Error())
			}
			for _, w := range r.Warnings {
				logger.Warn("rule profile: stored custom rule warning",
					"orgId", orgID, "ruleId", row.RuleID, "reason", w.Reason, "field", w.Field, "detail", w.Error())
			}
		}
		if r.Rule != nil {
			out = append(out, r.Rule)
		}
	}
	return out
}

// loggedOnceMax bounds loggedOnce; past it the set starts over, and a rule logs once more.
const loggedOnceMax = 4096

// loggedOnce remembers the stored rules whose problems were logged, by org, row and stored config, so a rule that
// is skipped or tolerated logs once per content per process instead of on every resolve (every 30 s per org and
// replica); an edit logs again.
type loggedOnce struct {
	mu   sync.Mutex
	seen map[[sha256.Size]byte]struct{}
}

var orgRulesLogged = &loggedOnce{}

// first reports whether row's content is new to l, and remembers it.
func (l *loggedOnce) first(orgID string, row *storageif.OrgCustomRule) bool {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(orgID), []byte(row.ID), row.Config} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[key]; ok {
		return false
	}
	if l.seen == nil || len(l.seen) >= loggedOnceMax {
		l.seen = make(map[[sha256.Size]byte]struct{})
	}
	l.seen[key] = struct{}{}
	return true
}
