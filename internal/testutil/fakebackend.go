package testutil

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// FakeBackend is an in-memory StorageBackend for tests that follows the Postgres backend on every method it
// implements (the storage contract runs against it strictly): visibility and filters, optimistic versions, what a
// write stamps, defaults, ordering, page bounds, ErrNotFound, and the ON DELETE CASCADE of the rows it keeps.
// Inserts are checked against the foreign keys of the collaborator, version, comment, invitation and settings rows;
// the older stores (triage, tokens, alerts, analysis) take any parent id. The analytics reads are empty on purpose
// (see their block).
//
// Tests may seed or inspect the exported maps directly, which bypasses those rules (e.g. to give a flow a
// timestamp SaveFlow would stamp itself). The methods are safe for concurrent use; direct map access is not.
type FakeBackend struct {
	// mu guards the maps and slices below; usage rows, audit rows and idempotency keys have their own locks,
	// and the read counters are atomic.
	mu sync.RWMutex

	Flows map[string]*interfaces.FlowDocument
	// Settings is the deployment slot (app_settings). UserSettings and OrgSettings are the per-user and per-org
	// slots: a user without one reads the application defaults, an org without one reads {Version: 1}.
	Settings     *interfaces.AppSettings
	UserSettings map[string]*interfaces.AppSettings
	OrgSettings  map[string]*interfaces.AppSettings
	PingErr      error
	// Conversations stores chat history keyed by scope+"\x00"+flowID; nil-safe
	// (the methods below tolerate a zero-valued struct-literal FakeBackend).
	Conversations map[string][]interfaces.ChatMessage
	// convVersions numbers the conversations (fakebackend_conversations_version.go).
	convVersions conversationVersions
	// OrgChannels backs the in-memory org-channel store (R2-3 tests).
	OrgChannels map[string]*interfaces.OrgChannel
	// ciCredentials backs the organisations' CI credentials (fakebackend_cicredentials.go), keyed by key id.
	ciCredentials map[string]*interfaces.CICredential
	// ciDeliveries is the CI webhook's replay record (fakebackend_cicredentials.go): when each pair was recorded.
	ciDeliveries map[ciDelivery]time.Time
	// FieldEncryptionOff models a Postgres backend without a field encryption key: secrets that must be sealed at
	// rest (CI credentials) are refused with ErrSecretSealingUnavailable.
	FieldEncryptionOff bool
	// OrgCustomRules backs the in-memory per-org custom-rule store (R4 tests).
	OrgCustomRules map[string]*interfaces.OrgCustomRule
	// KnowledgeDocs / KnowledgeChunks back the in-memory knowledge methods
	// (upload/replace/re-index round-trips in rag service tests).
	KnowledgeDocs   map[string]*interfaces.KnowledgeDocument
	KnowledgeChunks map[string]interfaces.KnowledgeChunk
	// EmbeddingDim is the deployment's vector width: a chunk counts as
	// vector-indexed (CountKnowledgeChunks, the sample's ordering) only when
	// its embedding has this width, mirroring the Postgres embedding_vec
	// rule. 0 keeps the any-embedding rule.
	EmbeddingDim int
	// UpdateKnowledgeEmbeddingsFailAt, when >0, makes the Nth (1-based) call
	// of UpdateKnowledgeChunkEmbeddings fail without writing — one re-index
	// page's commit failing; every other call writes (each call is its own
	// committed page). UpdateKnowledgeEmbeddingsCalls counts the calls.
	UpdateKnowledgeEmbeddingsFailAt int
	UpdateKnowledgeEmbeddingsCalls  int
	// FindingStatuses (flowID -> findingKey -> status) and Baselines (flowID ->
	// baseline) back the triage methods; lazily initialized for a struct literal.
	FindingStatuses map[string]map[string]*interfaces.FindingStatus
	Baselines       map[string]*interfaces.FlowBaseline
	// BatchSetFindingStatusErr, when non-nil, makes BatchSetFindingStatus fail
	// immediately. BatchSetFindingStatusFailAt, when >0, makes it fail at the
	// 0-based item index (proves the atomicity contract — items 0..K-1 must
	// NOT persist on a mid-batch failure).
	BatchSetFindingStatusErr    error
	BatchSetFindingStatusFailAt int
	// APITokens is keyed by token ID. Lazily initialized.
	APITokens map[string]*interfaces.APIToken
	// Users is keyed by user id; DeletedUsers lists the ids DeleteUser erased, in order.
	Users        map[string]*interfaces.User
	DeletedUsers []string
	// userAvatars mirrors user_avatars (user id -> image), cascading with the account; lazily initialized.
	userAvatars map[string]*interfaces.UserAvatar
	// Collaborators is flowID -> userID -> grant; reads take the email from Users, as Postgres joins it.
	Collaborators map[string]map[string]*interfaces.Collaborator
	// FlowVersions is flowID -> snapshots, oldest first.
	FlowVersions map[string][]*interfaces.FlowVersion
	// FindingComments is keyed by comment id.
	FindingComments map[string]*interfaces.FindingComment
	// Policies is keyed by orgID+"\x00"+policyID (the table's primary key).
	Policies map[string]*models.Policy
	// UserTokens is keyed by token hash (password reset / email verify). Lazily
	// initialized.
	UserTokens map[string]*interfaces.UserToken
	// FlowHealth is the persisted per-flow analysis summary (flowID -> snapshot) SaveFlowAnalysis writes; tests
	// may seed it.
	FlowHealth map[string]*interfaces.HealthSnapshot
	// DailyUsage / UsageErr let tests drive GetDailyUsage (e.g. to verify the
	// AI budget check fails closed on a store error).
	DailyUsage float64
	UsageErr   error
	// UsageMetrics collects every SaveUsageMetric row in arrival order (the
	// audited provider records asynchronously, hence the mutex); read via
	// SavedUsageMetrics.
	UsageMetrics []*interfaces.UsageMetric
	usageMu      sync.Mutex
	// ShareTokens is keyed by token hash; lazily initialized.
	ShareTokens map[string]*interfaces.ShareToken
	// GovernanceAlerts backs the governance-alert inbox methods (id -> alert).
	// Lazily initialized; the methods are in-memory mirrors of the real store.
	GovernanceAlerts map[string]*interfaces.GovernanceAlert
	// RuleNotices backs the "new rules" notices (id -> notice, fakebackend_notices.go). Lazily initialized.
	RuleNotices map[string]*interfaces.RuleNotice
	// snapshotRuleSets holds the rule set each FlowHealth snapshot recorded (flowID -> set), for RuleSetsByDigest.
	snapshotRuleSets map[string]*models.RuleSet
	// Orgs backs the organisation store (id -> org, members inline). Lazily initialized.
	Orgs map[string]*interfaces.Organisation
	// OrgInvites backs the invitation store (id -> invite, fakebackend_orgs.go). Lazily initialized.
	OrgInvites map[string]*interfaces.OrgInvite
	// refreshTokens is keyed by jti.
	refreshTokens map[string]*fakeRefreshToken
	// idem backs the IdempotencyStore (fake_idempotency.go).
	idem fakeIdempotency
	// jobRuns backs the background jobs' run state (fakebackend_jobs.go).
	jobRuns fakeJobRuns
	// flowSnapshots backs the cloud undo ring (fakebackend_flow_snapshots.go).
	flowSnapshots fakeFlowSnapshots
	// identityLinks backs the identity links (fakebackend_lookups.go).
	identityLinks fakeIdentityLinks
	// auditEvents keeps every saved audit row (the audit pool and detached writes save concurrently, hence
	// auditMu); read via SavedAuditEvents.
	auditEvents []*interfaces.AuditEvent
	auditMu     sync.Mutex

	// Read counters for what one governance sweep costs (scanner tests):
	// each is incremented on every call and never reset; atomic, since
	// services load flows concurrently. ListFlowsContentBytes sums the
	// Content bytes ListFlows returned — a MetadataOnly page ships none.
	// ListFlowsFilterLog returns every ListFlows filter in call order.
	ListFlowsCalls             atomic.Int64
	ListFlowsContentBytes      atomic.Int64
	LoadFlowCalls              atomic.Int64
	GetFlowBaselineCalls       atomic.Int64
	GetFlowBaselinesBatchCalls atomic.Int64
	LoadFlowHealthCalls        atomic.Int64
	LoadFlowHealthBatchCalls   atomic.Int64
	LoadOrgCalls               atomic.Int64
	ListCollaboratorsCalls     atomic.Int64
	RuleSetsByDigestCalls      atomic.Int64
	listFlowsFilters           []interfaces.FlowFilter // guarded by readsMu
	readsMu                    sync.Mutex
}

type fakeRefreshToken struct {
	info    interfaces.RefreshTokenInfo
	revoked bool
}

// ListFlowsFilterLog returns a copy of every filter ListFlows was called
// with, in call order.
func (m *FakeBackend) ListFlowsFilterLog() []interfaces.FlowFilter {
	m.readsMu.Lock()
	defer m.readsMu.Unlock()
	return append([]interfaces.FlowFilter(nil), m.listFlowsFilters...)
}

func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		Flows:         make(map[string]*interfaces.FlowDocument),
		Conversations: make(map[string][]interfaces.ChatMessage),
	}
}

func convKey(flowID, scope string) string { return scope + "\x00" + flowID }

// errForeignKey answers an insert whose parent row does not exist, which Postgres's foreign key rejects.
func errForeignKey(table, parent, id string) error {
	return fmt.Errorf("fake: insert into %s violates its foreign key: %s %q does not exist", table, parent, id)
}

// validFlowID is the Postgres backend's flow-id rule: '/' would nest one flow's blobs inside another's prefix.
func validFlowID(id string) error {
	if id == "" {
		return errors.New("flow id must not be empty")
	}
	if strings.Contains(id, "/") {
		return fmt.Errorf("invalid flow id %q: must not contain '/'", id)
	}
	return nil
}

// viaJSON converts v into a T through its JSON encoding, the way a JSONB column hands a value back.
func viaJSON[T any](v any) (T, error) {
	var out T
	data, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

// jsonClone deep-copies v the same way.
func jsonClone[T any](v T) (T, error) { return viaJSON[T](v) }

func (m *FakeBackend) Ping(_ context.Context) error { return m.PingErr }
func (m *FakeBackend) Close() error                 { return nil }

// CurrentSchemaVersion has no schema to report: 0, which the admin health page leaves out.
func (m *FakeBackend) CurrentSchemaVersion(context.Context) (int, error) { return 0, nil }

// ---- Flows ----

func cloneFlow(f *interfaces.FlowDocument) *interfaces.FlowDocument {
	cp := *f
	cp.Content = bytes.Clone(f.Content)
	cp.Tags = slices.Clone(f.Tags)
	return &cp
}

// UpdateFlowTags replaces the stored tags without a version bump; it stamps UpdatedAt.
func (m *FakeBackend) UpdateFlowTags(_ context.Context, flowID string, tags []string) error {
	normalized, err := interfaces.NormalizeFlowTags(tags)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.Flows[flowID]
	if !ok {
		return interfaces.ErrNotFound
	}
	row := cloneFlow(f)
	row.Tags = nil
	if len(normalized) > 0 {
		row.Tags = normalized
	}
	row.UpdatedAt = time.Now().UTC()
	m.Flows[flowID] = row
	return nil
}

// SaveFlow is the Postgres upsert: a new flow is stored at version 0 whatever f.Version says, an existing one
// updates only when f.Version equals the stored version (else ErrVersionConflict) and then bumps it. The save
// stamps UpdatedAt (and CreatedAt on insert); an update keeps the owner, organisation, creation time and tags,
// and an insert stores no tags (UpdateFlowTags sets them). f gets the new UpdatedAt and Version.
func (m *FakeBackend) SaveFlow(_ context.Context, f *interfaces.FlowDocument) error {
	if err := validFlowID(f.ID); err != nil {
		return err
	}
	content := f.Content
	if len(content) == 0 {
		content = json.RawMessage("{}")
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Flows == nil {
		m.Flows = make(map[string]*interfaces.FlowDocument)
	}
	var row *interfaces.FlowDocument
	if existing, ok := m.Flows[f.ID]; ok {
		if f.Version != existing.Version {
			return interfaces.ErrVersionConflict
		}
		row = cloneFlow(existing)
		row.Version = existing.Version + 1
	} else {
		row = &interfaces.FlowDocument{
			ID: f.ID, OwnerID: f.OwnerID, OrganizationID: f.OrganizationID, CreatedAt: now,
		}
	}
	row.Name, row.Description, row.Source, row.Metadata = f.Name, f.Description, f.Source, f.Metadata
	row.Content = bytes.Clone(content)
	row.UpdatedAt = now
	m.Flows[f.ID] = row
	f.UpdatedAt = now
	f.Version = row.Version
	return nil
}

func (m *FakeBackend) LoadFlow(_ context.Context, id string) (*interfaces.FlowDocument, error) {
	m.LoadFlowCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if f, ok := m.Flows[id]; ok {
		return cloneFlow(f), nil
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) LoadFlowHeader(_ context.Context, id string) (*interfaces.FlowDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if f, ok := m.Flows[id]; ok {
		cp := cloneFlow(f)
		cp.Content = nil
		cp.Source = "" // like both real backends: a header carries no document
		return cp, nil
	}
	return nil, interfaces.ErrNotFound
}

// matchFlowsLocked is Postgres's flowFilterWhere over the stored flows, unordered: the scope (AllFlows, SharedOnly,
// the user's owned/org/collaborator union, or the org scopes) and the Query, Created*, AfterID and Tags filters.
// An unscoped filter, SharedOnly without a user, or an invalid tag matches nothing. ListFlows, CountFlows and
// SearchFlowContents share it, as the Postgres statements do. The caller holds m.mu.
func (m *FakeBackend) matchFlowsLocked(filter interfaces.FlowFilter) []*interfaces.FlowDocument {
	var tags []string
	if len(filter.Tags) > 0 {
		normalized, err := interfaces.NormalizeFlowTags(filter.Tags)
		if err != nil || len(normalized) == 0 {
			return nil
		}
		tags = normalized
	}
	unscoped := filter.UserID == "" && filter.OrganizationID == "" && len(filter.OrganizationIDs) == 0
	if !filter.AllFlows && (unscoped || (filter.SharedOnly && filter.UserID == "")) {
		return nil
	}
	query := strings.ToLower(filter.Query)
	var out []*interfaces.FlowDocument
	for _, f := range m.Flows {
		switch {
		case !m.flowInScopeLocked(f, filter),
			query != "" && !strings.Contains(strings.ToLower(f.Name), query),
			filter.CreatedAfter != nil && !f.CreatedAt.After(*filter.CreatedAfter),
			filter.CreatedBefore != nil && !f.CreatedAt.Before(*filter.CreatedBefore),
			filter.AfterID != "" && f.ID <= filter.AfterID,
			tags != nil && !slices.ContainsFunc(tags, func(t string) bool { return slices.Contains(f.Tags, t) }):
			continue
		}
		out = append(out, f)
	}
	return out
}

func (m *FakeBackend) flowInScopeLocked(f *interfaces.FlowDocument, filter interfaces.FlowFilter) bool {
	switch {
	case filter.AllFlows:
		return true
	case filter.SharedOnly:
		return f.OwnerID != filter.UserID && m.Collaborators[f.ID][filter.UserID] != nil
	case filter.UserID != "":
		return f.OwnerID == filter.UserID ||
			(filter.OrganizationID != "" && f.OrganizationID == filter.OrganizationID) ||
			slices.Contains(filter.OrganizationIDs, f.OrganizationID) ||
			m.Collaborators[f.ID][filter.UserID] != nil
	case filter.OrganizationID != "":
		return f.OrganizationID == filter.OrganizationID
	default:
		return slices.Contains(filter.OrganizationIDs, f.OrganizationID)
	}
}

// sortFlows is Postgres's flowOrderBy, every order ending in the id; names compare lower-cased in byte order
// (Postgres uses the database collation).
func sortFlows(flows []*interfaces.FlowDocument, by interfaces.FlowSort) {
	sort.Slice(flows, func(i, j int) bool {
		a, b := flows[i], flows[j]
		switch by {
		case interfaces.FlowSortIDAsc:
		case interfaces.FlowSortUpdatedAsc:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.Before(b.UpdatedAt)
			}
		case interfaces.FlowSortNameAsc, interfaces.FlowSortNameDesc:
			if an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name); an != bn {
				return (an < bn) == (by == interfaces.FlowSortNameAsc)
			}
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		case interfaces.FlowSortBlocksDesc:
			if a.Metadata.BlockCount != b.Metadata.BlockCount {
				return a.Metadata.BlockCount > b.Metadata.BlockCount
			}
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		default:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		}
		return a.ID < b.ID
	})
}

// listFlows is one ListFlows page plus the number of flows the filter matches: Postgres's page bounds (a limit
// <= 0 or > 500 is 100) and MetadataOnly (no Content or Source).
func (m *FakeBackend) listFlows(filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, int, error) {
	if filter.Offset < 0 {
		return nil, 0, errors.New("OFFSET must not be negative")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	matched := m.matchFlowsLocked(filter)
	sortFlows(matched, filter.SortBy)
	start := min(filter.Offset, len(matched))
	var page []*interfaces.FlowDocument
	for _, f := range matched[start:min(start+limit, len(matched))] {
		cp := cloneFlow(f)
		if filter.MetadataOnly {
			cp.Content = nil
			cp.Source = ""
		}
		page = append(page, cp)
	}
	return page, len(matched), nil
}

func (m *FakeBackend) ListFlows(_ context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	m.ListFlowsCalls.Add(1)
	m.readsMu.Lock()
	m.listFlowsFilters = append(m.listFlowsFilters, filter)
	m.readsMu.Unlock()
	page, _, err := m.listFlows(filter)
	for _, f := range page {
		m.ListFlowsContentBytes.Add(int64(len(f.Content)))
	}
	return page, err
}

// CountFlows counts what ListFlows would page through, ignoring Limit and Offset.
func (m *FakeBackend) CountFlows(_ context.Context, filter interfaces.FlowFilter) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.matchFlowsLocked(filter)), nil
}

// DeleteFlow removes the flow and what cascades from it in Postgres (versions, analysis, triage, baseline,
// comments, share links, alerts, collaborators) plus its conversations, which have no foreign key and are deleted
// first, even for a missing flow (ErrNotFound).
func (m *FakeBackend) DeleteFlow(_ context.Context, id string) error {
	if err := validFlowID(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.Conversations {
		if _, flowID, _ := strings.Cut(key, "\x00"); flowID == id {
			delete(m.Conversations, key)
		}
	}
	if _, ok := m.Flows[id]; !ok {
		return interfaces.ErrNotFound
	}
	m.deleteFlowLocked(id)
	return nil
}

func (m *FakeBackend) deleteFlowLocked(id string) {
	delete(m.Flows, id)
	delete(m.FlowVersions, id)
	m.dropFlowSnapshots(id)
	delete(m.FlowHealth, id)
	delete(m.snapshotRuleSets, id)
	delete(m.FindingStatuses, id)
	delete(m.Baselines, id)
	delete(m.Collaborators, id)
	maps.DeleteFunc(m.FindingComments, func(_ string, c *interfaces.FindingComment) bool { return c.FlowID == id })
	maps.DeleteFunc(m.ShareTokens, func(_ string, t *interfaces.ShareToken) bool { return t.FlowID == id })
	maps.DeleteFunc(m.GovernanceAlerts, func(_ string, a *interfaces.GovernanceAlert) bool { return a.FlowID == id })
}

// SearchFlowContents is the Postgres content search: a case-insensitive substring of the flow's search body (its
// content's "source" string when set, else the content) within ListFlows's filter, newest first, metadata only.
func (m *FakeBackend) SearchFlowContents(_ context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return []*interfaces.FlowDocument{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.FlowDocument
	for _, f := range m.matchFlowsLocked(filter) {
		if strings.Contains(strings.ToLower(flowSearchBody(f.Content)), needle) {
			cp := cloneFlow(f)
			cp.Content = nil
			cp.Source = ""
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// flowSearchBody is flow_search.body: the content's "source" text unless it is empty or absent, else the content.
func flowSearchBody(content json.RawMessage) string {
	var doc map[string]json.RawMessage
	if json.Unmarshal(content, &doc) == nil {
		if raw, ok := doc["source"]; ok && string(raw) != "null" {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return string(raw)
			}
			if s != "" {
				return s
			}
		}
	}
	return string(content)
}

// ---- Settings ----

func (m *FakeBackend) SaveSettings(_ context.Context, s *interfaces.AppSettings) error {
	cp, err := jsonClone(s)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Settings = cp
	return nil
}

func (m *FakeBackend) LoadSettings(_ context.Context) (*interfaces.AppSettings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Settings == nil {
		return &interfaces.AppSettings{Version: 1}, nil
	}
	return jsonClone(m.Settings)
}

// SaveUserSettings writes the user's own slot; the user must exist (user_settings' foreign key).
func (m *FakeBackend) SaveUserSettings(_ context.Context, userID string, s *interfaces.AppSettings) error {
	cp, err := jsonClone(s)
	if err != nil {
		return fmt.Errorf("marshal user settings: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Users[userID] == nil {
		return errForeignKey("user_settings", "user", userID)
	}
	if m.UserSettings == nil {
		m.UserSettings = make(map[string]*interfaces.AppSettings)
	}
	m.UserSettings[userID] = cp
	return nil
}

// LoadUserSettings reads the user's slot, or the application defaults when they never saved one.
func (m *FakeBackend) LoadUserSettings(_ context.Context, userID string) (*interfaces.AppSettings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.UserSettings[userID]; ok {
		return jsonClone(s)
	}
	return viaJSON[*interfaces.AppSettings](models.DefaultSettings())
}

// SaveOrgSettings writes the org's slot; the org must exist (org_settings' foreign key).
func (m *FakeBackend) SaveOrgSettings(_ context.Context, orgID string, s *interfaces.AppSettings) error {
	cp, err := jsonClone(s)
	if err != nil {
		return fmt.Errorf("marshal org settings: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Orgs[orgID] == nil {
		return errForeignKey("org_settings", "organisation", orgID)
	}
	if m.OrgSettings == nil {
		m.OrgSettings = make(map[string]*interfaces.AppSettings)
	}
	m.OrgSettings[orgID] = cp
	return nil
}

// LoadOrgSettings reads the org's slot, or {Version: 1} when it has none.
func (m *FakeBackend) LoadOrgSettings(_ context.Context, orgID string) (*interfaces.AppSettings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.OrgSettings[orgID]; ok {
		return jsonClone(s)
	}
	return &interfaces.AppSettings{Version: 1}, nil
}

// ---- Conversations ----

func (m *FakeBackend) SaveConversation(_ context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	cp, err := jsonClone(messages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err = m.storeConversationLocked(convKey(flowID, scope), cp)
	return err
}

// LoadConversation returns an empty, non-nil slice when there is no conversation (or it was saved as nil).
func (m *FakeBackend) LoadConversation(_ context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	msgs, err := jsonClone(m.Conversations[convKey(flowID, scope)])
	if err != nil {
		return nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgs == nil {
		msgs = []interfaces.ChatMessage{}
	}
	return msgs, nil
}

func (m *FakeBackend) DeleteConversation(_ context.Context, flowID, scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Conversations, convKey(flowID, scope))
	m.convVersions.drop(convKey(flowID, scope))
	return nil
}

// ---- Users ----

// cloneUser copies a user row; the projection helpers below drop what a Postgres read does not select.
func cloneUser(u *interfaces.User) *interfaces.User {
	cp := *u
	if u.LockedUntil != nil {
		t := *u.LockedUntil
		cp.LockedUntil = &t
	}
	return &cp
}

// listedUser is the list projection (no password hash).
func listedUser(u *interfaces.User) *interfaces.User {
	cp := cloneUser(u)
	cp.Password = ""
	return cp
}

// sortedUsersLocked returns the users newest first, then by id (the Postgres list order).
func (m *FakeBackend) sortedUsersLocked(keep func(*interfaces.User) bool) []*interfaces.User {
	var out []*interfaces.User
	for _, u := range m.Users {
		if keep == nil || keep(u) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// emailTakenLocked reports whether a user other than id holds email (users.email is unique, case-sensitively).
func (m *FakeBackend) emailTakenLocked(email, id string) bool {
	for _, u := range m.Users {
		if u.Email == email && u.ID != id {
			return true
		}
	}
	return false
}

// SaveUser upserts by id: CreatedAt defaults to now and survives an update, UpdatedAt is stamped (both on user);
// another user's email is ErrEmailExists.
func (m *FakeBackend) SaveUser(_ context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.emailTakenLocked(user.Email, user.ID) {
		return interfaces.ErrEmailExists
	}
	if m.Users == nil {
		m.Users = make(map[string]*interfaces.User)
	}
	row := cloneUser(user)
	if existing, ok := m.Users[user.ID]; ok {
		row.CreatedAt = existing.CreatedAt
	}
	m.Users[user.ID] = row
	return nil
}

// CreateUser inserts a new user; a taken email or id is ErrEmailExists (any unique violation maps to it). The
// first user becomes an email-verified admin, but only with auth.WithAllowBootstrap on ctx (the registration
// path); the role and verification it stored are written back to user.
func (m *FakeBackend) CreateUser(ctx context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Users[user.ID]; ok || m.emailTakenLocked(user.Email, user.ID) {
		return interfaces.ErrEmailExists
	}
	row := cloneUser(user)
	if len(m.Users) == 0 && auth.AllowBootstrap(ctx) {
		row.Role = auth.RoleAdmin
		row.EmailVerified = true
	}
	if m.Users == nil {
		m.Users = make(map[string]*interfaces.User)
	}
	m.Users[user.ID] = row
	user.Role = row.Role
	user.EmailVerified = row.EmailVerified
	return nil
}

// LoadUserByEmail lower-cases the address it is given; stored addresses compare as stored.
func (m *FakeBackend) LoadUserByEmail(_ context.Context, email string) (*interfaces.User, error) {
	email = strings.ToLower(email)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.Users {
		if u.Email == email {
			return cloneUser(u), nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) LoadUserByID(_ context.Context, id string) (*interfaces.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if u, ok := m.Users[id]; ok {
		return cloneUser(u), nil
	}
	return nil, interfaces.ErrNotFound
}

// LoadUsersByIDs resolves the ids it can (duplicates and empty ids skipped) with the Postgres projection: no
// password hash, display name or avatar.
func (m *FakeBackend) LoadUsersByIDs(_ context.Context, ids []string) (map[string]*interfaces.User, error) {
	out := make(map[string]*interfaces.User, len(ids))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range ids {
		if u, ok := m.Users[id]; ok && id != "" {
			cp := listedUser(u)
			cp.DisplayName, cp.AvatarURL = "", ""
			out[id] = cp
		}
	}
	return out, nil
}

func (m *FakeBackend) CountUsers(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Users), nil
}

// ListUsers pages the users newest first; a limit <= 0 or > 500 is 100.
func (m *FakeBackend) ListUsers(_ context.Context, limit, offset int) ([]*interfaces.User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		return nil, errors.New("list users: OFFSET must not be negative")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := m.sortedUsersLocked(nil)
	start := min(offset, len(all))
	var out []*interfaces.User
	for _, u := range all[start:min(start+limit, len(all))] {
		out = append(out, listedUser(u))
	}
	return out, nil
}

func (m *FakeBackend) ListAdmins(_ context.Context) ([]*interfaces.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.User
	for _, u := range m.sortedUsersLocked(func(u *interfaces.User) bool { return u.Role == auth.RoleAdmin }) {
		out = append(out, listedUser(u))
	}
	return out, nil
}

// QueryUsers is the admin list: UserQuery's filters and order, a limit <= 0 of 100 and above 500 of 500.
func (m *FakeBackend) QueryUsers(_ context.Context, q interfaces.UserQuery) ([]*interfaces.User, int, error) {
	switch {
	case q.Limit <= 0:
		q.Limit = 100
	case q.Limit > 500:
		q.Limit = 500
	}
	q.Offset = max(q.Offset, 0)
	m.mu.RLock()
	all := make([]*interfaces.User, 0, len(m.Users))
	for _, u := range m.Users {
		all = append(all, listedUser(u))
	}
	m.mu.RUnlock()
	page, total := q.Apply(all)
	return page, total, nil
}

func (m *FakeBackend) LoadUserStats(_ context.Context) (*interfaces.UserStats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return interfaces.UserStatsOf(slices.Collect(maps.Values(m.Users)), time.Now()), nil
}

// updateUserLocked applies fn to the stored user; false when there is none.
func (m *FakeBackend) updateUserLocked(id string, fn func(*interfaces.User)) bool {
	u, ok := m.Users[id]
	if !ok {
		return false
	}
	row := cloneUser(u)
	fn(row)
	m.Users[id] = row
	return true
}

func (m *FakeBackend) UpdateUserRole(_ context.Context, id string, role auth.Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.updateUserLocked(id, func(u *interfaces.User) { u.Role, u.UpdatedAt = role, time.Now().UTC() }) {
		return interfaces.ErrNotFound
	}
	return nil
}

// UpdateUserPassword also clears the lockout; an unknown id is not an error.
func (m *FakeBackend) UpdateUserPassword(_ context.Context, id, passwordHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateUserLocked(id, func(u *interfaces.User) {
		u.Password, u.FailedLoginAttempts, u.LockedUntil, u.UpdatedAt = passwordHash, 0, nil, time.Now().UTC()
	})
	return nil
}

func (m *FakeBackend) UpdateUserProfile(_ context.Context, id, displayName, avatarURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.updateUserLocked(id, func(u *interfaces.User) {
		u.DisplayName, u.AvatarURL, u.UpdatedAt = displayName, avatarURL, time.Now().UTC()
	}) {
		return interfaces.ErrNotFound
	}
	return nil
}

// SaveUserAvatar mirrors the foreign key: storing an avatar for a user who does not exist is ErrNotFound.
func (m *FakeBackend) SaveUserAvatar(_ context.Context, id, mimeType string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Users[id]; !ok {
		return interfaces.ErrNotFound
	}
	if m.userAvatars == nil {
		m.userAvatars = make(map[string]*interfaces.UserAvatar)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.userAvatars[id] = &interfaces.UserAvatar{MimeType: mimeType, Data: cp, UpdatedAt: time.Now().UTC()}
	return nil
}

func (m *FakeBackend) LoadUserAvatar(_ context.Context, id string) (*interfaces.UserAvatar, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.userAvatars[id]
	if !ok || a == nil {
		return nil, interfaces.ErrNotFound
	}
	cp := *a
	cp.Data = append([]byte(nil), a.Data...)
	return &cp, nil
}

// DeleteUserAvatar is idempotent, like Postgres's DELETE.
func (m *FakeBackend) DeleteUserAvatar(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.userAvatars, id)
	return nil
}

// DeleteUser erases the account as Postgres's erase steps do (internal/storage/database/lifecycle_*.go), for the rows
// the fake keeps: the owned flows go with their conversations and their cascade; the user's id is cleared where
// rows stay with other people's content (versions, triage updater and assignee, baselines, comment author id and
// name, share links, invitations sent, CI credentials minted, organisations owned) and teammates' comment and assignment alerts naming the
// user say "a teammate"; alerts addressed to the user, the user's notices, sessions, usage rows, memberships,
// invitations to the user's email, settings, tokens and grants go; audit rows are anonymised, and pre-auth rows
// naming the user's address lose it. Organisations stay with their other members. A missing user is not an error.
func (m *FakeBackend) DeleteUser(_ context.Context, id string) error {
	m.idem.deleteUser(id)
	m.identityLinks.deleteUser(id)
	m.mu.Lock()
	var email, displayName string
	if u, ok := m.Users[id]; ok {
		email, displayName = u.Email, u.DisplayName
	}
	// Before the comment and triage identities go: they say which flows the user worked on.
	m.eraseActorFromAlertsLocked(id, email, displayName)
	for key := range m.Conversations {
		if _, flowID, _ := strings.Cut(key, "\x00"); m.Flows[flowID] != nil && m.Flows[flowID].OwnerID == id {
			delete(m.Conversations, key)
		}
	}
	for flowID, f := range m.Flows {
		if f.OwnerID == id {
			m.deleteFlowLocked(flowID)
		}
	}
	for _, versions := range m.FlowVersions {
		for _, v := range versions {
			if v.CreatedBy == id {
				v.CreatedBy = ""
			}
		}
	}
	for _, byKey := range m.FindingStatuses {
		for _, st := range byKey {
			if st.UpdatedBy == id {
				st.UpdatedBy = ""
			}
			if st.AssigneeID == id {
				st.AssigneeID = ""
			}
		}
	}
	for _, bl := range m.Baselines {
		if bl.CreatedBy == id {
			bl.CreatedBy = ""
		}
	}
	for _, c := range m.FindingComments {
		if c.AuthorID == id {
			c.AuthorID, c.AuthorName = "", ""
		}
	}
	for _, t := range m.ShareTokens {
		if t.CreatedBy == id {
			t.CreatedBy = ""
		}
	}
	for _, inv := range m.OrgInvites {
		if inv.InvitedBy == id {
			inv.InvitedBy = ""
		}
	}
	m.eraseCICredentialCreatorLocked(id)
	m.eraseChannelRecipientLocked(id)
	maps.DeleteFunc(m.GovernanceAlerts, func(_ string, a *interfaces.GovernanceAlert) bool { return a.TargetUser == id })
	maps.DeleteFunc(m.RuleNotices, func(_ string, n *interfaces.RuleNotice) bool {
		return n.Scope == interfaces.RuleNoticeScopeUser && n.ScopeID == id
	})
	maps.DeleteFunc(m.refreshTokens, func(_ string, t *fakeRefreshToken) bool { return t.info.UserID == id })
	for orgID, org := range m.Orgs {
		member := slices.ContainsFunc(org.Members, func(mem interfaces.OrgMember) bool { return mem.UserID == id })
		if member || org.OwnerID == id {
			cp := cloneOrg(org)
			cp.Members = slices.DeleteFunc(cp.Members, func(mem interfaces.OrgMember) bool { return mem.UserID == id })
			if cp.OwnerID == id {
				cp.OwnerID = ""
			}
			m.Orgs[orgID] = cp
		}
	}
	if email != "" {
		maps.DeleteFunc(m.OrgInvites, func(_ string, inv *interfaces.OrgInvite) bool { return inv.Email == email })
	}
	delete(m.UserSettings, id)
	delete(m.userAvatars, id)
	maps.DeleteFunc(m.APITokens, func(_ string, t *interfaces.APIToken) bool { return t.UserID == id })
	maps.DeleteFunc(m.UserTokens, func(_ string, t *interfaces.UserToken) bool { return t.UserID == id })
	for _, grants := range m.Collaborators {
		delete(grants, id)
	}
	if _, ok := m.Users[id]; ok {
		delete(m.Users, id)
		m.DeletedUsers = append(m.DeletedUsers, id)
	}
	m.mu.Unlock()

	m.usageMu.Lock()
	m.UsageMetrics = slices.DeleteFunc(m.UsageMetrics, func(u *interfaces.UsageMetric) bool { return u.UserID == id })
	m.usageMu.Unlock()
	m.auditMu.Lock()
	for _, e := range m.auditEvents {
		if email != "" && e.ResourceType == "email" && strings.EqualFold(e.ResourceID, email) {
			e.ResourceID, e.UserAgent = "", ""
		}
		if e.UserID != id && (e.ResourceType != "user" || e.ResourceID != id) {
			continue
		}
		if e.UserID == id {
			e.Email, e.IP, e.TokenID, e.UserID = "", "", "", ""
		}
		e.UserAgent = ""
	}
	m.auditMu.Unlock()
	return nil
}

// eraseActorFromAlertsLocked is Postgres's eraseActorFromAlerts: a teammate's comment alert reading exactly "New
// comment from <name>", or assignment alert reading exactly "Assigned to you by <name>", the name being the user's
// display name or email, on a flow the user reached (a grant, a membership, a comment they wrote or a finding they
// decided), says "a teammate" instead. Call it before any of those go.
func (m *FakeBackend) eraseActorFromAlertsLocked(id, email, displayName string) {
	prefix := map[string]string{"comment_added": "New comment from ", "finding_assigned": "Assigned to you by "}
	for _, a := range m.GovernanceAlerts {
		p, ok := prefix[a.Type]
		if !ok || a.TargetUser == "" || a.TargetUser == id {
			continue
		}
		if !m.flowReachableLocked(a.FlowID, id) && !m.workedOnLocked(a.FlowID, id) {
			continue
		}
		if (displayName != "" && a.Message == p+displayName) || (email != "" && a.Message == p+email) {
			a.Message = p + "a teammate"
		}
	}
}

// workedOnLocked reports whether userID wrote a comment on the flow or decided (or was assigned) one of its findings.
func (m *FakeBackend) workedOnLocked(flowID, userID string) bool {
	for _, c := range m.FindingComments {
		if c.FlowID == flowID && c.AuthorID == userID {
			return true
		}
	}
	for _, st := range m.FindingStatuses[flowID] {
		if st.UpdatedBy == userID || st.AssigneeID == userID {
			return true
		}
	}
	return false
}

// ExportUserData is Postgres's bundle (lifecycle_export.go) of the rows the fake keeps: the flows the user owns in id
// order, with the chat on them (flow, then scope) and the user's triage decisions on them (flow, then finding); the
// comments the user wrote on any flow (oldest first); the organisations the user belongs to, each carrying only the
// user's own membership; settings, audit trail, API tokens and sessions. Lists are [] when empty, never null; the
// Azure registry is empty (the fake keeps none). ErrNotFound for an unknown user; a settings read error fails.
func (m *FakeBackend) ExportUserData(ctx context.Context, id string) (*interfaces.UserDataExport, error) {
	out := &interfaces.UserDataExport{}
	var err error
	if out.User, err = m.LoadUserByID(ctx, id); err != nil {
		return nil, fmt.Errorf("export: user: %w", err)
	}
	if avatar, aerr := m.LoadUserAvatar(ctx, id); aerr == nil {
		out.Avatar = avatar
	} else if !errors.Is(aerr, interfaces.ErrNotFound) {
		return nil, fmt.Errorf("export: avatar: %w", aerr)
	}
	if out.Settings, err = m.LoadUserSettings(ctx, id); err != nil {
		return nil, fmt.Errorf("export: settings: %w", err)
	}
	out.AuditEvents = interfaces.NewExportedAuditEvents(m.matchingAuditEvents(interfaces.AuditFilter{UserID: id}))
	if out.APITokens, err = m.ListAPITokens(ctx, id); err != nil {
		return nil, fmt.Errorf("export: apiTokens: %w", err)
	}
	orgs, err := m.ListOrgsForUser(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("export: organisations: %w", err)
	}
	out.Organisations = make([]*interfaces.Organisation, 0, len(orgs))
	for _, o := range orgs {
		o.Members = slices.DeleteFunc(o.Members, func(mem interfaces.OrgMember) bool { return mem.UserID != id })
		out.Organisations = append(out.Organisations, o)
	}
	if out.Sessions, err = m.ListUserRefreshTokens(ctx, id); err != nil {
		return nil, fmt.Errorf("export: sessions: %w", err)
	}
	if out.Sessions == nil {
		out.Sessions = []interfaces.RefreshTokenInfo{}
	}
	out.AzureRegistry = interfaces.NewExportedAzureRegistry(nil, nil, nil)

	m.mu.RLock()
	defer m.mu.RUnlock()
	out.Flows = []*interfaces.FlowDocument{}
	owned := map[string]bool{}
	for _, f := range m.Flows {
		if f.OwnerID == id {
			out.Flows = append(out.Flows, cloneFlow(f))
			owned[f.ID] = true
		}
	}
	slices.SortFunc(out.Flows, func(a, b *interfaces.FlowDocument) int { return strings.Compare(a.ID, b.ID) })
	out.Conversations = []*interfaces.ExportedConversation{}
	for key, msgs := range m.Conversations {
		scope, flowID, _ := strings.Cut(key, "\x00")
		if !owned[flowID] {
			continue
		}
		cp, err := jsonClone(msgs)
		if err != nil {
			return nil, fmt.Errorf("export: conversations: %w", err)
		}
		out.Conversations = append(out.Conversations, &interfaces.ExportedConversation{FlowID: flowID, Scope: scope, Messages: cp})
	}
	slices.SortFunc(out.Conversations, func(a, b *interfaces.ExportedConversation) int {
		return cmp.Or(strings.Compare(a.FlowID, b.FlowID), strings.Compare(a.Scope, b.Scope))
	})
	out.Comments = []*interfaces.FindingComment{}
	for _, c := range m.FindingComments {
		if c.AuthorID == id {
			cp := *c
			out.Comments = append(out.Comments, &cp)
		}
	}
	slices.SortFunc(out.Comments, func(a, b *interfaces.FindingComment) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	out.TriageDecisions = []*interfaces.FindingStatus{}
	for flowID, byKey := range m.FindingStatuses {
		for _, st := range byKey {
			if owned[flowID] && st.UpdatedBy == id {
				cp := *st
				out.TriageDecisions = append(out.TriageDecisions, &cp)
			}
		}
	}
	slices.SortFunc(out.TriageDecisions, func(a, b *interfaces.FindingStatus) int {
		return cmp.Or(strings.Compare(a.FlowID, b.FlowID), strings.Compare(a.FindingKey, b.FindingKey))
	})
	out.ExportedAt = time.Now().UTC()
	return out, nil
}

// PurgeExpiredData removes what the Postgres retention sweep removes from the rows the fake keeps: expired
// sessions, API tokens, user tokens, share links, idempotency keys and invitations (and those accepted more than
// seven days ago), CI credentials expired more than 30 days ago, CI delivery records older than an hour, and with auditRetentionDays > 0 the audit, usage and alert rows older than that.
func (m *FakeBackend) PurgeExpiredData(_ context.Context, auditRetentionDays int) (*interfaces.PurgeResult, error) {
	now := time.Now()
	out := &interfaces.PurgeResult{IdempotencyKeys: m.idem.purge()}
	m.mu.Lock()
	out.RefreshTokens = deleteWhere(m.refreshTokens, func(t *fakeRefreshToken) bool { return t.info.ExpiresAt.Before(now) })
	out.APITokens = deleteWhere(m.APITokens, func(t *interfaces.APIToken) bool { return t.ExpiresAt != nil && t.ExpiresAt.Before(now) })
	out.UserTokens = deleteWhere(m.UserTokens, func(t *interfaces.UserToken) bool { return t.ExpiresAt.Before(now) })
	out.ShareTokens = deleteWhere(m.ShareTokens, func(t *interfaces.ShareToken) bool { return t.ExpiresAt != nil && t.ExpiresAt.Before(now) })
	out.OrgInvites = deleteWhere(m.OrgInvites, func(inv *interfaces.OrgInvite) bool {
		return inv.ExpiresAt.Before(now) || (inv.AcceptedAt != nil && inv.AcceptedAt.Before(now.AddDate(0, 0, -7)))
	})
	out.CICredentials = m.purgeCICredentialsLocked(now)
	out.CIDeliveries = m.purgeCIDeliveriesLocked(now)
	out.FlowSnapshots = m.purgeFlowSnapshots(now)
	if auditRetentionDays <= 0 {
		m.mu.Unlock()
		return out, nil
	}
	cutoff := now.AddDate(0, 0, -auditRetentionDays)
	out.GovAlerts = deleteWhere(m.GovernanceAlerts, func(a *interfaces.GovernanceAlert) bool { return a.CreatedAt.Before(cutoff) })
	out.RuleNotices = deleteWhere(m.RuleNotices, func(n *interfaces.RuleNotice) bool { return n.UpdatedAt.Before(cutoff) })
	m.mu.Unlock()

	m.usageMu.Lock()
	n := len(m.UsageMetrics)
	m.UsageMetrics = slices.DeleteFunc(m.UsageMetrics, func(u *interfaces.UsageMetric) bool { return u.CreatedAt.Before(cutoff) })
	out.UsageMetrics = n - len(m.UsageMetrics)
	m.usageMu.Unlock()
	m.auditMu.Lock()
	n = len(m.auditEvents)
	m.auditEvents = slices.DeleteFunc(m.auditEvents, func(e *interfaces.AuditEvent) bool { return e.CreatedAt.Before(cutoff) })
	out.AuditEvents = n - len(m.auditEvents)
	m.auditMu.Unlock()
	return out, nil
}

// deleteWhere deletes the entries of rows that del accepts and returns how many.
func deleteWhere[V any](rows map[string]V, del func(V) bool) int {
	n := len(rows)
	maps.DeleteFunc(rows, func(_ string, v V) bool { return del(v) })
	return n - len(rows)
}

// ---- Organisations ----

func cloneOrg(o *interfaces.Organisation) *interfaces.Organisation {
	cp := *o
	cp.Members = slices.Clone(o.Members)
	return &cp
}

// SaveOrg upserts the org and reconciles its members to org.Members: CreatedAt defaults to now and survives an
// update, UpdatedAt is stamped (both on org); a member keeps the JoinedAt it joined with (a new member's zero
// JoinedAt is now). A user listed twice fails, as the multi-row upsert does.
func (m *FakeBackend) SaveOrg(_ context.Context, org *interfaces.Organisation) error {
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Orgs == nil {
		m.Orgs = make(map[string]*interfaces.Organisation)
	}
	row := &interfaces.Organisation{ID: org.ID, Name: org.Name, OwnerID: org.OwnerID, CreatedAt: org.CreatedAt, UpdatedAt: now}
	var existing []interfaces.OrgMember
	if stored, ok := m.Orgs[org.ID]; ok {
		row.CreatedAt = stored.CreatedAt
		existing = stored.Members
	}
	members, err := reconcileRoster(existing, org.Members, now)
	if err != nil {
		return err
	}
	row.Members = members
	m.Orgs[org.ID] = row
	return nil
}

func (m *FakeBackend) LoadOrg(_ context.Context, id string) (*interfaces.Organisation, error) {
	m.LoadOrgCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if org, ok := m.Orgs[id]; ok {
		return cloneOrg(org), nil
	}
	return nil, interfaces.ErrNotFound
}

// ListOrgsForUser returns the orgs userID is a member of, most recently updated first, each with its members in
// joining order (never nil).
func (m *FakeBackend) ListOrgsForUser(_ context.Context, userID string) ([]*interfaces.Organisation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.Organisation
	for _, org := range m.Orgs {
		if slices.ContainsFunc(org.Members, func(mem interfaces.OrgMember) bool { return mem.UserID == userID }) {
			cp := cloneOrg(org)
			if cp.Members == nil {
				cp.Members = []interfaces.OrgMember{}
			}
			sort.SliceStable(cp.Members, func(i, j int) bool { return cp.Members[i].JoinedAt.Before(cp.Members[j].JoinedAt) })
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteOrg deletes the organisation as Postgres's org-delete steps do (internal/storage/database/lifecycle_*.go), for
// the rows the fake keeps: its knowledge documents (with their chunks), policies, channels, custom rules, notices and
// CI credentials go, its flows lose the org id, and the organisation goes with its members, invitations and settings; alerts, usage
// and audit rows keep the id as history. Idempotent.
func (m *FakeBackend) DeleteOrg(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for docID, d := range m.KnowledgeDocs {
		if d.OrgID == id {
			m.deleteKnowledgeDocumentLocked(id, docID)
		}
	}
	maps.DeleteFunc(m.Policies, func(_ string, p *models.Policy) bool { return p.OrgID == id })
	maps.DeleteFunc(m.OrgChannels, func(_ string, ch *interfaces.OrgChannel) bool { return ch.OrgID == id })
	maps.DeleteFunc(m.OrgCustomRules, func(_ string, r *interfaces.OrgCustomRule) bool { return r.OrgID == id })
	maps.DeleteFunc(m.RuleNotices, func(_ string, n *interfaces.RuleNotice) bool {
		return n.Scope == interfaces.RuleNoticeScopeOrg && n.ScopeID == id
	})
	m.deleteOrgCICredentialsLocked(id)
	for _, f := range m.Flows {
		if f.OrganizationID == id {
			f.OrganizationID = ""
		}
	}
	delete(m.Orgs, id)
	delete(m.OrgSettings, id)
	maps.DeleteFunc(m.OrgInvites, func(_ string, inv *interfaces.OrgInvite) bool { return inv.OrgID == id })
	return nil
}

// ---- Sharing ----

// AddCollaborator upserts the grant (GrantedAt defaults to now, on c); the flow and the user must exist.
func (m *FakeBackend) AddCollaborator(_ context.Context, flowID string, c *interfaces.Collaborator) error {
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Flows[flowID] == nil {
		return errForeignKey("flow_collaborators", "flow", flowID)
	}
	if m.Users[c.UserID] == nil {
		return errForeignKey("flow_collaborators", "user", c.UserID)
	}
	if m.Collaborators == nil {
		m.Collaborators = make(map[string]map[string]*interfaces.Collaborator)
	}
	if m.Collaborators[flowID] == nil {
		m.Collaborators[flowID] = make(map[string]*interfaces.Collaborator)
	}
	m.Collaborators[flowID][c.UserID] = &interfaces.Collaborator{UserID: c.UserID, Permission: c.Permission, GrantedAt: c.GrantedAt}
	return nil
}

// collaboratorsLocked lists a flow's grants oldest first, each with its user's current email; a grant whose user
// is gone is dropped like the inner join drops it. The result is nil when there are none.
func (m *FakeBackend) collaboratorsLocked(flowID string) []*interfaces.Collaborator {
	var out []*interfaces.Collaborator
	for userID, c := range m.Collaborators[flowID] {
		u, ok := m.Users[userID]
		if !ok {
			continue
		}
		cp := *c
		cp.Email = u.Email
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].GrantedAt.Equal(out[j].GrantedAt) {
			return out[i].GrantedAt.Before(out[j].GrantedAt)
		}
		return out[i].UserID < out[j].UserID
	})
	return out
}

// ListCollaborators returns up to limit grants (limit <= 0 = all), oldest first; never nil.
func (m *FakeBackend) ListCollaborators(_ context.Context, flowID string, limit int) ([]*interfaces.Collaborator, error) {
	m.ListCollaboratorsCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := m.collaboratorsLocked(flowID)
	if out == nil {
		out = []*interfaces.Collaborator{}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListCollaboratorsBatch maps each listed flow to its grants; flows without any are absent.
func (m *FakeBackend) ListCollaboratorsBatch(_ context.Context, flowIDs []string) (map[string][]*interfaces.Collaborator, error) {
	out := make(map[string][]*interfaces.Collaborator, len(flowIDs))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range flowIDs {
		if list := m.collaboratorsLocked(id); list != nil {
			out[id] = list
		}
	}
	return out, nil
}

func (m *FakeBackend) UpdateCollaborator(_ context.Context, flowID, userID, permission string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.Collaborators[flowID][userID]
	if !ok {
		return interfaces.ErrNotFound
	}
	cp := *c
	cp.Permission = permission
	m.Collaborators[flowID][userID] = &cp
	return nil
}

func (m *FakeBackend) RemoveCollaborator(_ context.Context, flowID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Collaborators[flowID][userID]; !ok {
		return interfaces.ErrNotFound
	}
	delete(m.Collaborators[flowID], userID)
	return nil
}

// ---- Usage tracking ----
func (m *FakeBackend) SaveUsageMetric(_ context.Context, metric *interfaces.UsageMetric) error {
	if metric == nil {
		return nil
	}
	cp := *metric
	m.usageMu.Lock()
	m.UsageMetrics = append(m.UsageMetrics, &cp)
	m.usageMu.Unlock()
	return nil
}

// SavedUsageMetrics returns a snapshot of the rows SaveUsageMetric received.
func (m *FakeBackend) SavedUsageMetrics() []*interfaces.UsageMetric {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	return append([]*interfaces.UsageMetric(nil), m.UsageMetrics...)
}
func (m *FakeBackend) GetDailyUsage(_ context.Context, _, _ string) (float64, error) {
	return m.DailyUsage, m.UsageErr
}

// ---- Dashboard ----

// SaveFlowAnalysis upserts the flow's health snapshot (AnalyzedAt defaults to now) and the rule set it records.
func (m *FakeBackend) SaveFlowAnalysis(_ context.Context, fa *interfaces.FlowAnalysis) error {
	at := fa.AnalyzedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FlowHealth == nil {
		m.FlowHealth = make(map[string]*interfaces.HealthSnapshot)
	}
	snap := &interfaces.HealthSnapshot{
		HealthScore: fa.HealthScore, Errors: fa.Errors, Warnings: fa.Warnings, Info: fa.Info, AnalyzedAt: at,
	}
	delete(m.snapshotRuleSets, fa.FlowID)
	if fa.RuleSet != nil {
		if m.snapshotRuleSets == nil {
			m.snapshotRuleSets = make(map[string]*models.RuleSet)
		}
		m.snapshotRuleSets[fa.FlowID] = cloneRuleSet(fa.RuleSet)
		snap.RuleSetDigest = fa.RuleSet.Digest
	}
	m.FlowHealth[fa.FlowID] = snap
	return nil
}

func (m *FakeBackend) LoadFlowHealth(_ context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	m.LoadFlowHealthCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if h, ok := m.FlowHealth[flowID]; ok {
		cp := *h
		return &cp, nil
	}
	return nil, nil
}
func (m *FakeBackend) LoadFlowHealthBatch(_ context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
	m.LoadFlowHealthBatchCalls.Add(1)
	out := make(map[string]*interfaces.HealthSnapshot, len(flowIDs))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range flowIDs {
		if h, ok := m.FlowHealth[id]; ok {
			cp := *h
			out[id] = &cp
		}
	}
	return out, nil
}

// The dashboard and analytics rollups below are deliberately empty results: they aggregate in SQL over most
// tables, the dashboard and analytics services are tested against Postgres, and an in-memory re-implementation
// would be a second, untested copy of those statements.

func (m *FakeBackend) FlowDashboardData(_ context.Context, _ string, _ int) (*interfaces.DashboardData, error) {
	return &interfaces.DashboardData{ByCategory: map[string]int{}}, nil
}
func (m *FakeBackend) FlowDashboardAdvanced(_ context.Context, _ string, _ int) (*interfaces.DashboardAdvancedData, error) {
	return &interfaces.DashboardAdvancedData{Security: interfaces.DashboardSecurity{}}, nil
}
func (m *FakeBackend) LoadAnalysisSnapshots(_ context.Context, _ []string) ([]interfaces.AnalysisSnapshotRow, error) {
	return []interfaces.AnalysisSnapshotRow{}, nil
}
func (m *FakeBackend) LoadAnalysisHistory(_ context.Context, _ interfaces.AnalyticsWindow) (*interfaces.AnalysisHistoryRows, error) {
	return &interfaces.AnalysisHistoryRows{}, nil
}
func (m *FakeBackend) LoadTriageAnalytics(_ context.Context, _ interfaces.AnalyticsWindow) (*interfaces.TriageRows, error) {
	return &interfaces.TriageRows{}, nil
}
func (m *FakeBackend) LoadUsageAnalytics(_ context.Context, _ string, _ []string, _ string, _ interfaces.AnalyticsWindow) (*interfaces.UsageRows, error) {
	return &interfaces.UsageRows{}, nil
}
func (m *FakeBackend) LoadActivityAnalytics(_ context.Context, _ string, _ interfaces.AnalyticsWindow, _ int) (*interfaces.ActivityRows, error) {
	return &interfaces.ActivityRows{}, nil
}
func (m *FakeBackend) LoadSecurityAnalytics(_ context.Context, _, _ string, _ bool) (*interfaces.SecurityRows, error) {
	return &interfaces.SecurityRows{}, nil
}

// ---- Refresh tokens ----

// StoreRefreshToken records an issued token; a jti already stored is left as it is.
func (m *FakeBackend) StoreRefreshToken(_ context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshTokens == nil {
		m.refreshTokens = make(map[string]*fakeRefreshToken)
	}
	if _, ok := m.refreshTokens[jti]; !ok {
		m.refreshTokens[jti] = &fakeRefreshToken{info: interfaces.RefreshTokenInfo{
			ID: jti, UserID: userID, CreatedAt: time.Now().UTC(), ExpiresAt: expiresAt.UTC(), UserAgent: userAgent, IP: ip,
		}}
	}
	return nil
}

// activeRefreshTokenLocked is the stored token when it is neither revoked nor expired.
func (m *FakeBackend) activeRefreshTokenLocked(jti string) *fakeRefreshToken {
	if t, ok := m.refreshTokens[jti]; ok && !t.revoked && t.info.ExpiresAt.After(time.Now()) {
		return t
	}
	return nil
}

func (m *FakeBackend) IsRefreshTokenValid(_ context.Context, jti string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeRefreshTokenLocked(jti) != nil, nil
}

// RevokeRefreshToken revokes jti; a missing or already revoked token is ErrTokenAlreadyRevoked.
func (m *FakeBackend) RevokeRefreshToken(_ context.Context, jti string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.refreshTokens[jti]
	if !ok || t.revoked {
		return interfaces.ErrTokenAlreadyRevoked
	}
	t.revoked = true
	return nil
}

// VerifyAndRevokeRefreshToken revokes an active token and returns it (ID, UserID, CreatedAt, ExpiresAt); a
// missing, revoked or expired one is ErrTokenAlreadyRevoked.
func (m *FakeBackend) VerifyAndRevokeRefreshToken(_ context.Context, jti string) (*interfaces.RefreshTokenInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.activeRefreshTokenLocked(jti)
	if t == nil {
		return nil, interfaces.ErrTokenAlreadyRevoked
	}
	t.revoked = true
	return &interfaces.RefreshTokenInfo{ID: t.info.ID, UserID: t.info.UserID, CreatedAt: t.info.CreatedAt, ExpiresAt: t.info.ExpiresAt}, nil
}

func (m *FakeBackend) RevokeUserRefreshTokens(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.refreshTokens {
		if t.info.UserID == userID {
			t.revoked = true
		}
	}
	return nil
}

// ListUserRefreshTokens returns the user's active sessions, newest first, without the user id.
func (m *FakeBackend) ListUserRefreshTokens(_ context.Context, userID string) ([]interfaces.RefreshTokenInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []interfaces.RefreshTokenInfo
	for jti, t := range m.refreshTokens {
		if t.info.UserID == userID && m.activeRefreshTokenLocked(jti) != nil {
			info := t.info
			info.UserID = ""
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RevokeRefreshTokenForUser revokes jti only when userID owns it and it is not revoked yet, else ErrNotFound.
func (m *FakeBackend) RevokeRefreshTokenForUser(_ context.Context, jti, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.refreshTokens[jti]
	if !ok || t.info.UserID != userID || t.revoked {
		return interfaces.ErrNotFound
	}
	t.revoked = true
	return nil
}

// ---- Knowledge Base ----
// KnowledgeDocs / KnowledgeChunks back the knowledge methods in memory;
// lazily initialized, so a struct-literal FakeBackend works without them.

// SaveKnowledgeDocument inserts the document stamped with the current time, as the column default does.
func (m *FakeBackend) SaveKnowledgeDocument(_ context.Context, _ string, doc *interfaces.KnowledgeDocument) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initKnowledge()
	if _, ok := m.KnowledgeDocs[doc.ID]; ok {
		return fmt.Errorf("fake: knowledge document %q already exists", doc.ID)
	}
	m.KnowledgeDocs[doc.ID] = &interfaces.KnowledgeDocument{ID: doc.ID, OrgID: doc.OrgID, Filename: doc.Filename, CreatedAt: time.Now().UTC(), ChunkerVersion: max(doc.ChunkerVersion, 1)}
	return nil
}
func (m *FakeBackend) DeleteKnowledgeDocument(_ context.Context, _ string, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteKnowledgeDocumentLocked(orgID, id)
	return nil
}
func (m *FakeBackend) deleteKnowledgeDocumentLocked(orgID, id string) {
	if d, ok := m.KnowledgeDocs[id]; ok && d.OrgID == orgID {
		delete(m.KnowledgeDocs, id)
		maps.DeleteFunc(m.KnowledgeChunks, func(_ string, c interfaces.KnowledgeChunk) bool { return c.DocID == id })
	}
}
func (m *FakeBackend) DeleteKnowledgeDocumentByName(_ context.Context, orgID, filename string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.KnowledgeDocs {
		if d.OrgID == orgID && d.Filename == filename {
			m.deleteKnowledgeDocumentLocked(orgID, id)
		}
	}
	return nil
}

// sortedDocsLocked returns the org's documents in creation order (newest first when newest), then by id.
func (m *FakeBackend) sortedDocsLocked(orgID string, newest bool) []*interfaces.KnowledgeDocument {
	var out []*interfaces.KnowledgeDocument
	for _, d := range m.KnowledgeDocs {
		if d.OrgID == orgID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt) == newest
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListKnowledgeDocuments pages the org's documents newest first (limit <= 0 = all) with their chunk and
// vector-indexed counts.
func (m *FakeBackend) ListKnowledgeDocuments(_ context.Context, orgID string, limit, offset int) ([]*interfaces.KnowledgeDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	docs := m.sortedDocsLocked(orgID, true)
	docs = docs[min(max(offset, 0), len(docs)):]
	if limit > 0 && len(docs) > limit {
		docs = docs[:limit]
	}
	var out []*interfaces.KnowledgeDocument
	for _, d := range docs {
		cp := *d
		for _, c := range m.KnowledgeChunks {
			if c.DocID == d.ID {
				cp.ChunkCount++
				if m.vectorIndexed(c) {
					cp.VectorIndexed++
				}
			}
		}
		out = append(out, &cp)
	}
	return out, nil
}

// ListKnowledgeDocumentIDsByFilename returns the ids of the org's documents named filename, oldest first, and the
// org's document count.
func (m *FakeBackend) ListKnowledgeDocumentIDsByFilename(_ context.Context, orgID, filename string) ([]string, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	docs := m.sortedDocsLocked(orgID, false)
	var ids []string
	for _, d := range docs {
		if d.Filename == filename {
			ids = append(ids, d.ID)
		}
	}
	return ids, len(docs), nil
}

// SaveKnowledgeChunks stores the chunks; a chunk without an org takes its document's.
func (m *FakeBackend) SaveKnowledgeChunks(_ context.Context, _ string, chunks []interfaces.KnowledgeChunk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initKnowledge()
	for _, c := range chunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && c.OrgID == "" {
			c.OrgID = d.OrgID
		}
		c.Embedding = slices.Clone(c.Embedding)
		m.KnowledgeChunks[c.ID] = c
	}
	return nil
}

// knowledgeMinSimilarity is the Postgres search's relevance floor (cosine similarity).
const knowledgeMinSimilarity = 0.5

// SearchKnowledge ranks the org's chunks whose embedding has the query's width by cosine similarity, most similar
// first, keeps those at or above the floor and returns up to limit with their document's filename (no
// embeddings), as the Postgres search does. identity keeps the chunks embedded under it and the legacy ones
// without an identity; "" keeps every chunk.
func (m *FakeBackend) SearchKnowledge(_ context.Context, orgID string, queryEmbedding []float32, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	type scored struct {
		chunk interfaces.KnowledgeChunk
		sim   float64
	}
	m.mu.RLock()
	var hits []scored
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || len(c.Embedding) != len(queryEmbedding) {
			continue
		}
		if identity != "" && c.EmbeddingIdentity != "" && c.EmbeddingIdentity != identity {
			continue
		}
		if sim := cosineSimilarity(c.Embedding, queryEmbedding); sim >= knowledgeMinSimilarity {
			hits = append(hits, scored{interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, Content: c.Content, Filename: d.Filename}, sim})
		}
	}
	m.mu.RUnlock()
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].sim != hits[j].sim {
			return hits[i].sim > hits[j].sim
		}
		return hits[i].chunk.ID < hits[j].chunk.ID
	})
	var out []interfaces.KnowledgeChunk
	for _, h := range hits[:min(max(limit, 0), len(hits))] {
		out = append(out, h.chunk)
	}
	return out, nil
}

func cosineSimilarity(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ListKnowledgeChunkContents mirrors the Postgres keyset page: the org's
// chunks strictly after the (DocID, ID) cursor, in (DocID, ID) order, capped
// at limit (limit <= 0 = all), skipping the chunks already embedded under
// notIdentity ("" skips none).
func (m *FakeBackend) ListKnowledgeChunkContents(_ context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int, notIdentity string) ([]interfaces.KnowledgeChunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID {
			continue
		}
		if c.DocID < after.DocID || (c.DocID == after.DocID && c.ID <= after.ID) {
			continue
		}
		if notIdentity != "" && c.EmbeddingIdentity == notIdentity {
			continue
		}
		out = append(out, interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, OrgID: orgID, Content: c.Content})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// vectorIndexed mirrors the Postgres embedding_vec rule: an embedded chunk
// is in the vector index when EmbeddingDim is unset or matches its width.
func (m *FakeBackend) vectorIndexed(c interfaces.KnowledgeChunk) bool {
	return len(c.Embedding) > 0 && (m.EmbeddingDim == 0 || len(c.Embedding) == m.EmbeddingDim)
}

// SampleKnowledgeChunkEmbeddings mirrors the Postgres sample: ONE chunk per
// document (the vector-indexed one first, then the lowest ID), documents
// whose pick is indexed first, then DocID order, capped at n — embeddings
// copied so callers cannot alias the stored slice.
func (m *FakeBackend) SampleKnowledgeChunkEmbeddings(_ context.Context, orgID string, n int) ([]interfaces.KnowledgeChunk, error) {
	if n <= 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID && len(c.Embedding) > 0 {
			all = append(all, interfaces.KnowledgeChunk{
				ID: c.ID, DocID: c.DocID, Content: c.Content,
				Embedding: append([]float32(nil), c.Embedding...),
			})
		}
	}
	// DISTINCT ON (doc_id) ... ORDER BY doc_id, (embedding_vec IS NULL), id
	sort.Slice(all, func(i, j int) bool {
		if all[i].DocID != all[j].DocID {
			return all[i].DocID < all[j].DocID
		}
		if vi, vj := m.vectorIndexed(all[i]), m.vectorIndexed(all[j]); vi != vj {
			return vi
		}
		return all[i].ID < all[j].ID
	})
	var out []interfaces.KnowledgeChunk
	for _, c := range all {
		if len(out) > 0 && out[len(out)-1].DocID == c.DocID {
			continue
		}
		out = append(out, c)
	}
	// ORDER BY unindexed, doc_id LIMIT n
	sort.SliceStable(out, func(i, j int) bool {
		if vi, vj := m.vectorIndexed(out[i]), m.vectorIndexed(out[j]); vi != vj {
			return vi
		}
		return out[i].DocID < out[j].DocID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// SampleKnowledgeChunkEmbeddingsUpTo mirrors the Postgres query: the org's
// embedded chunks at or before the (DocID, ID) cursor, nearest first, capped
// at n, embeddings copied.
func (m *FakeBackend) SampleKnowledgeChunkEmbeddingsUpTo(_ context.Context, orgID string, upTo interfaces.KnowledgeChunkCursor, n int) ([]interfaces.KnowledgeChunk, error) {
	if n <= 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || len(c.Embedding) == 0 {
			continue
		}
		if c.DocID > upTo.DocID || (c.DocID == upTo.DocID && c.ID > upTo.ID) {
			continue
		}
		out = append(out, interfaces.KnowledgeChunk{
			ID: c.ID, DocID: c.DocID, Content: c.Content,
			Embedding: append([]float32(nil), c.Embedding...),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID > out[j].DocID
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}
func (m *FakeBackend) UpdateKnowledgeChunkEmbeddings(_ context.Context, _ string, chunks []interfaces.KnowledgeChunk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initKnowledge()
	m.UpdateKnowledgeEmbeddingsCalls++
	if m.UpdateKnowledgeEmbeddingsFailAt > 0 && m.UpdateKnowledgeEmbeddingsCalls == m.UpdateKnowledgeEmbeddingsFailAt {
		return fmt.Errorf("fake: update knowledge chunk embeddings failed (call %d)", m.UpdateKnowledgeEmbeddingsCalls)
	}
	for _, c := range chunks {
		if old, ok := m.KnowledgeChunks[c.ID]; ok {
			old.Embedding = slices.Clone(c.Embedding)
			old.EmbeddingIdentity = c.EmbeddingIdentity
			m.KnowledgeChunks[c.ID] = old
		}
	}
	return nil
}
func (m *FakeBackend) CountKnowledgeChunks(_ context.Context, orgID string) (int, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total, indexed := 0, 0
	for _, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID {
			total++
			if m.vectorIndexed(c) {
				indexed++
			}
		}
	}
	return total, indexed, nil
}

// CountKnowledgeChunksByIdentity mirrors the Postgres census: the org's chunks at identity, at another one, and
// (by difference) without one; "" names nothing current or stranded.
func (m *FakeBackend) CountKnowledgeChunksByIdentity(_ context.Context, orgID, identity string) (interfaces.KnowledgeChunkCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var counts interfaces.KnowledgeChunkCounts
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID {
			continue
		}
		counts.Total++
		if m.vectorIndexed(c) {
			counts.VectorIndexed++
		}
		switch {
		case identity == "" || c.EmbeddingIdentity == "":
		case c.EmbeddingIdentity == identity:
			counts.Current++
		default:
			counts.Stranded++
		}
	}
	return counts, nil
}

// CountStrandedKnowledgeChunksByDocument mirrors the Postgres per-document count of chunks at another identity.
func (m *FakeBackend) CountStrandedKnowledgeChunksByDocument(_ context.Context, orgID, identity string) (map[string]int, error) {
	out := map[string]int{}
	if identity == "" {
		return out, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID && c.EmbeddingIdentity != "" && c.EmbeddingIdentity != identity {
			out[c.DocID]++
		}
	}
	return out, nil
}

// StampKnowledgeChunkIdentity rewrites the identity of the org's chunks at from ("" = none) to to.
func (m *FakeBackend) StampKnowledgeChunkIdentity(_ context.Context, _, orgID, from, to string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID && c.EmbeddingIdentity == from {
			c.EmbeddingIdentity = to
			m.KnowledgeChunks[id] = c
			n++
		}
	}
	return n, nil
}

func (m *FakeBackend) initKnowledge() {
	if m.KnowledgeDocs == nil {
		m.KnowledgeDocs = make(map[string]*interfaces.KnowledgeDocument)
	}
	if m.KnowledgeChunks == nil {
		m.KnowledgeChunks = make(map[string]interfaces.KnowledgeChunk)
	}
}

// ---- Finding triage & baselines ----

// upsertFindingStatusLocked is the Postgres upsert: UpdatedAt is now, and an empty justification keeps the stored
// one (a status change that omits the reason must not erase a risk acceptance).
func (m *FakeBackend) upsertFindingStatusLocked(byKey map[string]*interfaces.FindingStatus, st *interfaces.FindingStatus, now time.Time) {
	cp := *st
	cp.UpdatedAt = now
	if prev, ok := byKey[st.FindingKey]; ok && cp.Justification == "" {
		cp.Justification = prev.Justification
	}
	byKey[st.FindingKey] = &cp
}

func (m *FakeBackend) SetFindingStatus(_ context.Context, st *interfaces.FindingStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindingStatuses == nil {
		m.FindingStatuses = make(map[string]map[string]*interfaces.FindingStatus)
	}
	if m.FindingStatuses[st.FlowID] == nil {
		m.FindingStatuses[st.FlowID] = make(map[string]*interfaces.FindingStatus)
	}
	m.upsertFindingStatusLocked(m.FindingStatuses[st.FlowID], st, time.Now().UTC())
	return nil
}

// BatchSetFindingStatus mirrors the single-item upsert but stages updates in a
// local copy so an injected failure mid-batch leaves NO partial state behind
// (matching the Postgres atomicity contract — a non-atomic fake would hide
// real bugs).
func (m *FakeBackend) BatchSetFindingStatus(_ context.Context, flowID, _ string, items []*interfaces.FindingStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	staged := maps.Clone(m.FindingStatuses[flowID])
	if staged == nil {
		staged = make(map[string]*interfaces.FindingStatus, len(items))
	}
	now := time.Now().UTC()
	for i, st := range items {
		if m.BatchSetFindingStatusFailAt > 0 && i == m.BatchSetFindingStatusFailAt {
			return fmt.Errorf("injected failure at item %d", i)
		}
		if st == nil {
			return fmt.Errorf("batch item %d: nil", i)
		}
		st.FlowID = flowID
		m.upsertFindingStatusLocked(staged, st, now)
	}
	if m.BatchSetFindingStatusErr != nil {
		return m.BatchSetFindingStatusErr
	}
	if len(items) == 0 {
		return nil
	}
	if m.FindingStatuses == nil {
		m.FindingStatuses = make(map[string]map[string]*interfaces.FindingStatus)
	}
	m.FindingStatuses[flowID] = staged
	return nil
}

// ListFindingStatuses returns copies ordered by finding key; never nil.
func (m *FakeBackend) ListFindingStatuses(_ context.Context, flowID string) ([]*interfaces.FindingStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byKey := m.FindingStatuses[flowID]
	out := make([]*interfaces.FindingStatus, 0, len(byKey))
	for _, v := range byKey {
		cp := *v
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingKey < out[j].FindingKey })
	return out, nil
}

func (m *FakeBackend) DeleteFindingStatus(_ context.Context, flowID, findingKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.FindingStatuses[flowID], findingKey)
	return nil
}

func cloneBaseline(b *interfaces.FlowBaseline) *interfaces.FlowBaseline {
	cp := *b
	cp.Keys = slices.Clone(b.Keys)
	if cp.Keys == nil {
		cp.Keys = []string{}
	}
	cp.RuleSet = cloneRuleSet(b.RuleSet)
	return &cp
}

// GetFlowBaseline returns the flow's baseline (Keys never nil), or (nil, nil) without one.
func (m *FakeBackend) GetFlowBaseline(_ context.Context, flowID string) (*interfaces.FlowBaseline, error) {
	m.GetFlowBaselineCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b, ok := m.Baselines[flowID]; ok {
		return cloneBaseline(b), nil
	}
	return nil, nil
}

func (m *FakeBackend) GetFlowBaselinesBatch(_ context.Context, flowIDs []string) (map[string]*interfaces.FlowBaseline, error) {
	m.GetFlowBaselinesBatchCalls.Add(1)
	out := make(map[string]*interfaces.FlowBaseline, len(flowIDs))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range flowIDs {
		if b, ok := m.Baselines[id]; ok {
			cp := cloneBaseline(b)
			cp.RuleSet = nil // a batch read carries the digest only
			out[id] = cp
		}
	}
	return out, nil
}

// SetFlowBaseline replaces the flow's baseline, stamping CreatedAt with the current time on every save and deriving
// RuleSetDigest from the rule set it records.
func (m *FakeBackend) SetFlowBaseline(_ context.Context, b *interfaces.FlowBaseline) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Baselines == nil {
		m.Baselines = make(map[string]*interfaces.FlowBaseline)
	}
	cp := cloneBaseline(b)
	cp.CreatedAt = time.Now().UTC()
	cp.RuleSetDigest = ""
	if cp.RuleSet != nil {
		cp.RuleSetDigest = cp.RuleSet.Digest
	}
	m.Baselines[b.FlowID] = cp
	return nil
}

func (m *FakeBackend) ClearFlowBaseline(_ context.Context, flowID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Baselines, flowID)
	return nil
}

// ---- API tokens ----

// CreateAPIToken stores the token stamped with the current time (the column default; t is not changed).
func (m *FakeBackend) CreateAPIToken(_ context.Context, t *interfaces.APIToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.APITokens == nil {
		m.APITokens = make(map[string]*interfaces.APIToken)
	}
	cp := *t
	cp.CreatedAt = time.Now().UTC()
	cp.Scopes = slices.Clone(t.Scopes)
	m.APITokens[t.ID] = &cp
	return nil
}

func (m *FakeBackend) GetAPITokenByHash(_ context.Context, tokenHash string) (*interfaces.APIToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.APITokens {
		if t.TokenHash == tokenHash {
			cp := *t
			cp.Scopes = slices.Clone(t.Scopes)
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) ListAPITokens(_ context.Context, userID string) ([]*interfaces.APIToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*interfaces.APIToken, 0)
	for _, t := range m.APITokens {
		if t.UserID == userID {
			cp := *t
			cp.Scopes = slices.Clone(t.Scopes)
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *FakeBackend) DeleteAPIToken(_ context.Context, userID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.APITokens[id]; ok && t.UserID == userID {
		delete(m.APITokens, id)
	}
	return nil
}

func (m *FakeBackend) CreateUserToken(_ context.Context, t *interfaces.UserToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.UserTokens == nil {
		m.UserTokens = make(map[string]*interfaces.UserToken)
	}
	cp := *t
	m.UserTokens[t.TokenHash] = &cp
	return nil
}

func (m *FakeBackend) ConsumeUserToken(_ context.Context, purpose, tokenHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.UserTokens[tokenHash]
	if !ok || t.Purpose != purpose || time.Now().After(t.ExpiresAt) {
		return "", interfaces.ErrNotFound
	}
	delete(m.UserTokens, tokenHash)
	return t.UserID, nil
}

// SetUserEmailVerified marks the user verified (UpdatedAt untouched); an unknown id is not an error.
func (m *FakeBackend) SetUserEmailVerified(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateUserLocked(userID, func(u *interfaces.User) { u.EmailVerified = true })
	return nil
}

// InvalidateUserTokens mirrors the real backend: removes every unused token of
// the given purposes for userID — assertable directly on the fake's state.
func (m *FakeBackend) InvalidateUserTokens(_ context.Context, userID string, purposes ...string) error {
	if len(purposes) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, t := range m.UserTokens {
		if t.UserID == userID && slices.Contains(purposes, t.Purpose) {
			delete(m.UserTokens, hash)
		}
	}
	return nil
}

// ---- Audit log ----

// SaveAuditEvent keeps a copy of the row; an id stored already is a no-op that returns nil, as the Postgres
// INSERT ... ON CONFLICT (id) DO NOTHING is.
func (m *FakeBackend) SaveAuditEvent(_ context.Context, e *interfaces.AuditEvent) error {
	cp := *e
	cp.Meta = maps.Clone(e.Meta)
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	for _, have := range m.auditEvents {
		if have.ID == cp.ID {
			return nil
		}
	}
	m.auditEvents = append(m.auditEvents, &cp)
	return nil
}

// SavedAuditEvents returns copies of the saved audit rows in arrival order.
func (m *FakeBackend) SavedAuditEvents() []*interfaces.AuditEvent {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	out := make([]*interfaces.AuditEvent, len(m.auditEvents))
	for i, e := range m.auditEvents {
		cp := *e
		cp.Meta = maps.Clone(e.Meta)
		out[i] = &cp
	}
	return out
}

// ListAuditEvents mirrors the Postgres list: the filters, newest first with the id breaking ties, and the page
// bounds (limit <= 0 is 100, capped at 500).
func (m *FakeBackend) ListAuditEvents(_ context.Context, f interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 500:
		limit = 500
	}
	out := m.matchingAuditEvents(f)
	offset := min(max(f.Offset, 0), len(out))
	return out[offset:min(offset+limit, len(out))], nil
}

// matchingAuditEvents returns every saved row f's filters match, in ListAuditEvents's order (never nil).
func (m *FakeBackend) matchingAuditEvents(f interfaces.AuditFilter) []*interfaces.AuditEvent {
	out := []*interfaces.AuditEvent{}
	for _, e := range m.SavedAuditEvents() {
		subject := f.IncludeSubject && e.ResourceType == "user" && e.ResourceID == f.UserID
		if f.UserID != "" && e.UserID != f.UserID && !subject {
			continue
		}
		if (f.Action != "" && e.Action != f.Action) || (f.Since != nil && e.CreatedAt.Before(*f.Since)) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---- Flow versioning ----

// maxFlowVersions is how many snapshots per flow Postgres keeps (maxFlowVersionSnapshots).
const maxFlowVersions = 50

// SaveFlowVersion stores a snapshot under the next number (v.Version is overwritten) and prunes the oldest beyond
// maxFlowVersions; the flow must exist.
func (m *FakeBackend) SaveFlowVersion(_ context.Context, v *interfaces.FlowVersion) error {
	if err := validFlowID(v.FlowID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Flows[v.FlowID] == nil {
		return fmt.Errorf("lock flow for versioning: flow %q not found", v.FlowID)
	}
	next := 1
	for _, versions := range m.FlowVersions {
		for _, existing := range versions {
			if existing.ID == v.ID {
				return fmt.Errorf("fake: flow version %q already exists", v.ID)
			}
		}
	}
	versions := m.FlowVersions[v.FlowID]
	if len(versions) > 0 {
		next = versions[len(versions)-1].Version + 1
	}
	v.Version = next
	cp := *v
	cp.Content = bytes.Clone(v.Content)
	if cp.Content == nil {
		cp.Content = json.RawMessage("{}")
	}
	if m.FlowVersions == nil {
		m.FlowVersions = make(map[string][]*interfaces.FlowVersion)
	}
	versions = append(versions, &cp)
	m.FlowVersions[v.FlowID] = versions[max(len(versions)-maxFlowVersions, 0):]
	return nil
}

// ListFlowVersions returns the newest snapshots first without their content; a limit <= 0 or > 200 is 50.
func (m *FakeBackend) ListFlowVersions(_ context.Context, flowID string, limit int) ([]*interfaces.FlowVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.FlowVersions[flowID]
	out := []*interfaces.FlowVersion{}
	for i := len(versions) - 1; i >= 0 && len(out) < limit; i-- {
		cp := *versions[i]
		cp.Content = nil
		out = append(out, &cp)
	}
	return out, nil
}

func (m *FakeBackend) LoadFlowVersion(_ context.Context, flowID string, version int) (*interfaces.FlowVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.FlowVersions[flowID] {
		if v.Version == version {
			cp := *v
			cp.Content = bytes.Clone(v.Content)
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

// ---- Finding comments ----

// AddFindingComment stamps a missing ID and CreatedAt (on c); the flow must exist.
func (m *FakeBackend) AddFindingComment(_ context.Context, c *interfaces.FindingComment) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Flows[c.FlowID] == nil {
		return errForeignKey("finding_comments", "flow", c.FlowID)
	}
	if _, ok := m.FindingComments[c.ID]; ok {
		return fmt.Errorf("fake: finding comment %q already exists", c.ID)
	}
	if m.FindingComments == nil {
		m.FindingComments = make(map[string]*interfaces.FindingComment)
	}
	cp := *c
	m.FindingComments[c.ID] = &cp
	return nil
}

// ListFindingComments returns the finding's thread oldest first (nil when empty).
func (m *FakeBackend) ListFindingComments(_ context.Context, flowID, findingKey string) ([]*interfaces.FindingComment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.FindingComment
	for _, c := range m.FindingComments {
		if c.FlowID == flowID && c.FindingKey == findingKey {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteFindingComment deletes the flow's comment when authorID wrote it; anyone else, an empty authorID included,
// is ErrNotCommentAuthor. An absent comment is a no-op.
func (m *FakeBackend) DeleteFindingComment(_ context.Context, flowID, commentID, authorID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.FindingComments[commentID]
	if !ok || c.FlowID != flowID {
		return nil
	}
	if authorID == "" || c.AuthorID != authorID {
		return interfaces.ErrNotCommentAuthor
	}
	delete(m.FindingComments, commentID)
	return nil
}

// DeleteFindingCommentAsModerator deletes the flow's comment whoever wrote it; the caller has authorized the
// moderator. An absent comment is a no-op.
func (m *FakeBackend) DeleteFindingCommentAsModerator(_ context.Context, flowID, commentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.FindingComments[commentID]; ok && c.FlowID == flowID {
		delete(m.FindingComments, commentID)
	}
	return nil
}

// ---- Share tokens ----

// CreateShareToken stamps a missing ID and CreatedAt (on t).
func (m *FakeBackend) CreateShareToken(_ context.Context, t *interfaces.ShareToken) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ShareTokens == nil {
		m.ShareTokens = make(map[string]*interfaces.ShareToken)
	}
	cp := *t
	m.ShareTokens[t.TokenHash] = &cp
	return nil
}

// GetShareTokenByHash resolves an unexpired link, else ErrNotFound.
func (m *FakeBackend) GetShareTokenByHash(_ context.Context, tokenHash string) (*interfaces.ShareToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.ShareTokens[tokenHash]
	if !ok || (t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now())) {
		return nil, interfaces.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// ListShareTokens returns the flow's unexpired links, newest first.
func (m *FakeBackend) ListShareTokens(_ context.Context, flowID string) ([]*interfaces.ShareToken, error) {
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.ShareToken
	for _, t := range m.ShareTokens {
		if t.FlowID == flowID && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) {
			cp := *t
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *FakeBackend) RevokeShareToken(_ context.Context, flowID, tokenID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.ShareTokens, func(_ string, t *interfaces.ShareToken) bool { return t.ID == tokenID && t.FlowID == flowID })
	return nil
}

// ---- Policies ----

func policyKey(orgID, id string) string { return orgID + "\x00" + id }

// SavePolicy upserts by (id, org) and writes the stamps back onto p. Like the Postgres row it keeps the name,
// description, rules, gate severity, waivers and severity budgets.
func (m *FakeBackend) SavePolicy(_ context.Context, p *models.Policy) error {
	row, err := clonePolicy(&models.Policy{
		ID: p.ID, OrgID: p.OrgID, Name: p.Name, Description: p.Description, Rules: p.Rules,
		GateSeverity: p.GateSeverity, Waivers: p.Waivers, MaxErrors: p.MaxErrors, MaxWarnings: p.MaxWarnings, MaxInfo: p.MaxInfo,
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row.CreatedAt, row.UpdatedAt = now, now
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Policies == nil {
		m.Policies = make(map[string]*models.Policy)
	}
	if existing, ok := m.Policies[policyKey(p.OrgID, p.ID)]; ok {
		row.CreatedAt = existing.CreatedAt
	}
	m.Policies[policyKey(p.OrgID, p.ID)] = row
	p.CreatedAt, p.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return nil
}

// clonePolicy copies p deeply: rules and waivers as their JSONB columns hand them back, budgets as new values.
func clonePolicy(p *models.Policy) (*models.Policy, error) {
	cp := *p
	rules, err := jsonClone(p.Rules)
	if err != nil {
		return nil, err
	}
	cp.Rules = rules
	if len(p.Waivers) > 0 {
		if cp.Waivers, err = jsonClone(p.Waivers); err != nil {
			return nil, err
		}
	} else {
		cp.Waivers = nil // none is NULL on Postgres
	}
	cloneInt := func(v *int) *int {
		if v == nil {
			return nil
		}
		n := *v
		return &n
	}
	cp.MaxErrors, cp.MaxWarnings, cp.MaxInfo = cloneInt(p.MaxErrors), cloneInt(p.MaxWarnings), cloneInt(p.MaxInfo)
	return &cp, nil
}

func (m *FakeBackend) GetPolicy(_ context.Context, orgID, id string) (*models.Policy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.Policies[policyKey(orgID, id)]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	return clonePolicy(p)
}

// ListPolicies returns the org's policies by name (nil when none).
func (m *FakeBackend) ListPolicies(_ context.Context, orgID string) ([]*models.Policy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*models.Policy
	for _, p := range m.Policies {
		if p.OrgID != orgID {
			continue
		}
		cp, err := clonePolicy(p)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *FakeBackend) DeletePolicy(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Policies, policyKey(orgID, id))
	return nil
}

// ---- Governance alerts ----

func (m *FakeBackend) govAlertsLocked() map[string]*interfaces.GovernanceAlert {
	if m.GovernanceAlerts == nil {
		m.GovernanceAlerts = make(map[string]*interfaces.GovernanceAlert)
	}
	return m.GovernanceAlerts
}

// govAlertVisibleLocked is Postgres's govAlertWhere: without a user only team-wide alerts; with one, the
// team-wide alerts and theirs, on flows the user can reach.
func (m *FakeBackend) govAlertVisibleLocked(a *interfaces.GovernanceAlert, userID string) bool {
	if userID == "" {
		return a.TargetUser == ""
	}
	if a.TargetUser != "" && a.TargetUser != userID {
		return false
	}
	return m.flowReachableLocked(a.FlowID, userID)
}

// flowReachableLocked reports whether userID owns the flow, collaborates on it or sees it through an org membership.
func (m *FakeBackend) flowReachableLocked(flowID, userID string) bool {
	f, ok := m.Flows[flowID]
	if !ok {
		return false
	}
	if f.OwnerID == userID || m.Collaborators[f.ID][userID] != nil {
		return true
	}
	org, ok := m.Orgs[f.OrganizationID]
	return ok && slices.ContainsFunc(org.Members, func(mem interfaces.OrgMember) bool { return mem.UserID == userID })
}

// RecordGovernanceAlert stores a new alert (CreatedAt defaults to now, on a); a known ID is left as it is.
func (m *FakeBackend) RecordGovernanceAlert(ctx context.Context, a *interfaces.GovernanceAlert) error {
	_, err := m.InsertGovernanceAlert(ctx, a)
	return err
}

// InsertGovernanceAlert is RecordGovernanceAlert reporting whether the alert was new.
func (m *FakeBackend) InsertGovernanceAlert(_ context.Context, a *interfaces.GovernanceAlert) (bool, error) {
	if a == nil || a.ID == "" {
		return false, fmt.Errorf("governance alert requires an id")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.govAlertsLocked()[a.ID]; ok {
		return false, nil
	}
	cp := *a
	cp.ReadAt, cp.DismissedAt = nil, nil
	m.GovernanceAlerts[a.ID] = &cp
	return true, nil
}

// ListGovernanceAlerts returns the caller's visible alerts newest first (id breaking ties); a limit <= 0 is 50.
func (m *FakeBackend) ListGovernanceAlerts(_ context.Context, filter interfaces.GovernanceAlertFilter) ([]*interfaces.GovernanceAlert, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*interfaces.GovernanceAlert, 0, len(m.GovernanceAlerts))
	for _, a := range m.GovernanceAlerts {
		if (!filter.IncludeDismissed && a.DismissedAt != nil) || !m.govAlertVisibleLocked(a, filter.UserID) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	start := min(max(filter.Offset, 0), len(out))
	return out[start:min(start+limit, len(out))], nil
}

func (m *FakeBackend) UnreadGovernanceAlertCount(ctx context.Context) (int, error) {
	return m.UnreadGovernanceAlertCountFor(ctx, "")
}

func (m *FakeBackend) UnreadGovernanceAlertCountFor(_ context.Context, userID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, a := range m.GovernanceAlerts {
		if a.ReadAt == nil && a.DismissedAt == nil && m.govAlertVisibleLocked(a, userID) {
			n++
		}
	}
	return n, nil
}

func (m *FakeBackend) MarkGovernanceAlertRead(_ context.Context, userID, alertID string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.GovernanceAlerts[alertID]; ok && a.ReadAt == nil && m.govAlertVisibleLocked(a, userID) {
		a.ReadAt = &now
	}
	return nil
}

func (m *FakeBackend) MarkAllGovernanceAlertsRead(_ context.Context, userID string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.GovernanceAlerts {
		if a.ReadAt == nil && a.DismissedAt == nil && m.govAlertVisibleLocked(a, userID) {
			a.ReadAt = &now
		}
	}
	return nil
}

func (m *FakeBackend) DismissGovernanceAlert(_ context.Context, userID, alertID string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.GovernanceAlerts[alertID]; ok && a.DismissedAt == nil && m.govAlertVisibleLocked(a, userID) {
		a.DismissedAt = &now
	}
	return nil
}

// ClearGovernanceAlerts deletes the caller's visible dismissed alerts.
func (m *FakeBackend) ClearGovernanceAlerts(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.GovernanceAlerts, func(_ string, a *interfaces.GovernanceAlert) bool {
		return a.DismissedAt != nil && m.govAlertVisibleLocked(a, userID)
	})
	return nil
}

// ---- Org custom rules and channels ----

// SaveOrgCustomRule backs the per-org custom-rule store in memory (R4 tests).
// The (OrgID, RuleID) uniqueness and the org-ownership guard mirror the
// Postgres upsert exactly — a fake laxer than the real backend would report
// success on precisely the cross-tenant write the real one rejects (B8). So do
// the rename guard and the refusal of a config that is not JSON (JSONB).
func (m *FakeBackend) SaveOrgCustomRule(_ context.Context, rule *interfaces.OrgCustomRule) error {
	if rule == nil || rule.ID == "" || rule.OrgID == "" || rule.RuleID == "" {
		return errors.New("org custom rule requires id, orgId and ruleId")
	}
	if !json.Valid(rule.Config) {
		return errors.New("save org custom rule: config is not JSON")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OrgCustomRules == nil {
		m.OrgCustomRules = make(map[string]*interfaces.OrgCustomRule)
	}
	// A row of this org under another rule id is a rename, refused before anything changes.
	if existing, ok := m.OrgCustomRules[rule.ID]; ok && existing.OrgID == rule.OrgID && existing.RuleID != rule.RuleID {
		return interfaces.ErrOrgCustomRuleRenamed
	}
	// Composite key wins: re-saving the same (org, ruleID) replaces in place.
	for _, existing := range m.OrgCustomRules {
		if existing.OrgID == rule.OrgID && existing.RuleID == rule.RuleID {
			rule.ID = existing.ID
			break
		}
	}
	if existing, ok := m.OrgCustomRules[rule.ID]; ok && existing.OrgID != rule.OrgID {
		return interfaces.ErrNotFound
	}
	cp := *rule
	m.OrgCustomRules[rule.ID] = &cp
	return nil
}

func (m *FakeBackend) DeleteOrgCustomRule(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.OrgCustomRules[id]; ok && r.OrgID == orgID {
		delete(m.OrgCustomRules, id)
	}
	return nil
}

func (m *FakeBackend) ListOrgCustomRules(_ context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgCustomRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.OrgCustomRule
	for _, r := range m.OrgCustomRules {
		if r.OrgID == orgID && (!enabledOnly || r.Enabled) {
			cp := *r
			out = append(out, &cp)
		}
	}
	// Deterministic order — the analysis path compiles these into a rule set
	// whose digest feeds the cache key; map-iteration order would make it
	// unstable across calls.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SaveOrgChannel backs the org-channel store in memory (R2-3 tests). The
// org-ownership check mirrors the Postgres upsert's
// `WHERE org_channels.org_id = EXCLUDED.org_id`: an id existing under a
// DIFFERENT org is ErrNotFound, never a silent cross-tenant overwrite. An
// update keeps the row's CreatedAt, as the upsert's SET list does.
func (m *FakeBackend) SaveOrgChannel(_ context.Context, ch *interfaces.OrgChannel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OrgChannels == nil {
		m.OrgChannels = make(map[string]*interfaces.OrgChannel)
	}
	cp := copyOrgChannel(ch)
	cp.Unreadable = false
	if existing, ok := m.OrgChannels[ch.ID]; ok {
		if existing.OrgID != ch.OrgID {
			return interfaces.ErrNotFound
		}
		cp.CreatedAt = existing.CreatedAt
	}
	m.OrgChannels[ch.ID] = cp
	return nil
}

func (m *FakeBackend) DeleteOrgChannel(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.OrgChannels[id]; ok && c.OrgID == orgID {
		delete(m.OrgChannels, id)
	}
	return nil
}

func (m *FakeBackend) ListOrgChannels(_ context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgChannel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.OrgChannel
	for _, c := range m.OrgChannels {
		if c.OrgID == orgID && (!enabledOnly || c.Enabled) {
			cp := copyOrgChannel(c)
			if cp.Unreadable {
				cp.URL, cp.Secret, cp.Secrets = "", "", nil
			}
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
