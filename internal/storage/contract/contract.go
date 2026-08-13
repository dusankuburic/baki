// Package contract provides a shared test suite that both StorageBackend
// implementations (filesystem and database/postgres) run against, so the two
// can't quietly drift apart on return-shape semantics like
//
//   - "not found" → nil vs interfaces.ErrNotFound
//   - "empty list" → nil slice vs non-nil empty slice
//
// The historical motivation: `LoadConversation` returned a non-nil empty
// slice on the filesystem backend and a `nil` slice on Postgres, so frontend
// nil-checks branched differently per deployment.
//
// This package is not a `_test` package: each storage-backend test file
// imports it and calls RunSuite from a Test* function. That way the same
// scenarios run in CI under both backends (Postgres is opt-in via
// DATABASE_URL).
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// findFlow returns the flow with the given id from a list, or nil.
func findFlow(flows []*interfaces.FlowDocument, id string) *interfaces.FlowDocument {
	for _, f := range flows {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// RunSuite executes the cross-backend contract scenarios against b. The
// suite seeds uuid-prefixed fixtures, so it is safe to re-run against a
// backend with existing state (e.g. a long-lived Postgres DB used across
// test sessions). The "first user becomes admin" scenario is skipped if
// the backend already has users.
func RunSuite(t *testing.T, b interfaces.StorageBackend) {
	t.Helper()
	ctx := context.Background()
	runID := uuid.New().String()[:8]

	t.Run("LoadConversation_missing_returns_empty_slice_not_nil", func(t *testing.T) {
		msgs, err := b.LoadConversation(ctx, "no-such-flow", "scope")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msgs == nil {
			t.Errorf("missing conversation: expected non-nil empty slice (matches filesystem semantics), got nil")
		}
		if len(msgs) != 0 {
			t.Errorf("missing conversation: expected empty, got %d messages", len(msgs))
		}
	})

	t.Run("LoadFlow_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadFlow(ctx, "no-such-flow-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing flow: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadUserByEmail_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByEmail(ctx, "no-such@example.com")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by email): expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadUserByID_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadUserByID(ctx, "no-such-user-id")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing user (by id): expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LoadOrg_missing_returns_ErrNotFound", func(t *testing.T) {
		_, err := b.LoadOrg(ctx, "no-such-org")
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing org: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("ListCollaborators_empty_returns_non_nil_slice", func(t *testing.T) {
		// Same nil-vs-empty contract as LoadConversation: callers should be
		// able to do `len(coll) == 0` without backend-specific nil checks.
		coll, err := b.ListCollaborators(ctx, "no-such-flow")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if coll == nil {
			t.Errorf("empty collaborators: expected non-nil empty slice, got nil")
		}
		if len(coll) != 0 {
			t.Errorf("empty collaborators: expected length 0, got %d", len(coll))
		}
	})

	t.Run("CreateUser_promotes_first_to_admin", func(t *testing.T) {
		// Only meaningful on a truly empty backend. Skip when prior state
		// exists (e.g. a Postgres DB shared across test runs).
		count, err := b.CountUsers(ctx)
		if err != nil {
			t.Fatalf("CountUsers: %v", err)
		}
		if count > 0 {
			t.Skip("backend has existing users; cannot exercise first-admin promotion")
		}

		// The bootstrap-admin rule is opt-in via context (set by the registration
		// path). Without the flag the first user must NOT be promoted — this is
		// the SSO-JIT path, which must not let whoever reaches SSO first claim
		// admin on a fresh deployment.
		noBootstrap := &interfaces.User{
			ID:       "contract-user-0-" + runID,
			Email:    "contract-0-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(ctx, noBootstrap); err != nil {
			t.Fatalf("CreateUser (no bootstrap flag): %v", err)
		}
		if string(noBootstrap.Role) == "admin" {
			t.Errorf("first user without bootstrap flag: expected non-admin, got admin (SSO-path must not auto-promote)")
		}

		// With the flag (registration path) but a NON-empty table, the rule
		// correctly does NOT fire either — confirms gating on BOTH flag AND
		// empty-table.
		flagged := &interfaces.User{
			ID:       "contract-user-1-" + runID,
			Email:    "contract-1-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(auth.WithAllowBootstrap(ctx, true), flagged); err != nil {
			t.Fatalf("CreateUser (with bootstrap flag, non-empty table): %v", err)
		}
		if string(flagged.Role) == "admin" {
			t.Errorf("non-first user: expected non-admin, got admin (table was not empty)")
		}
	})

	t.Run("CreateUser_duplicate_email_returns_ErrEmailExists", func(t *testing.T) {
		email := "contract-dup-" + runID + "@example.com"
		u := &interfaces.User{
			ID:       "contract-user-dup-a-" + runID,
			Email:    email,
			Password: "hash",
		}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser first: %v", err)
		}
		dup := &interfaces.User{
			ID:       "contract-user-dup-b-" + runID,
			Email:    email,
			Password: "hash",
		}
		err := b.CreateUser(ctx, dup)
		if !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("duplicate email: expected ErrEmailExists, got %v", err)
		}
	})

	t.Run("SaveConversation_then_LoadConversation_round_trips", func(t *testing.T) {
		flowID := "contract-flow-" + runID
		msgs := []interfaces.ChatMessage{
			{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)},
			{ID: "m2", Role: "assistant", Content: "hello", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil {
			t.Fatalf("LoadConversation: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("round-trip: want 2 messages, got %d", len(got))
		}
	})

	t.Run("DeleteConversation_removes_and_is_idempotent", func(t *testing.T) {
		flowID := "contract-delconv-" + runID
		msgs := []interfaces.ChatMessage{
			{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		if err := b.DeleteConversation(ctx, flowID, "scope"); err != nil {
			t.Fatalf("DeleteConversation: %v", err)
		}
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil {
			t.Fatalf("LoadConversation after delete: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after delete: want 0 messages, got %d", len(got))
		}
		// Deleting a missing conversation is a no-op, not an error.
		if err := b.DeleteConversation(ctx, flowID, "scope"); err != nil {
			t.Errorf("DeleteConversation (missing) should be a no-op, got %v", err)
		}
	})

	t.Run("SaveUserSettings_then_LoadUserSettings_round_trips", func(t *testing.T) {
		userID := "contract-usettings-" + runID
		// Seed the user first: Postgres enforces user_settings.user_id → users(id)
		// via a foreign key. (The filesystem backend has no such constraint, but
		// seeding keeps the test valid on both backends.)
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: userID, Email: "usettings-" + runID + "@example.com", Password: "h",
		}); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		settings := &interfaces.AppSettings{
			Version: 1,
			General: interfaces.GeneralSettings{CheckForUpdates: "daily"},
		}
		if err := b.SaveUserSettings(ctx, userID, settings); err != nil {
			t.Fatalf("SaveUserSettings: %v", err)
		}
		got, err := b.LoadUserSettings(ctx, userID)
		if err != nil {
			t.Fatalf("LoadUserSettings: %v", err)
		}
		if got.General.CheckForUpdates != "daily" {
			t.Errorf("round-trip: want daily, got %s", got.General.CheckForUpdates)
		}
	})

	t.Run("SaveOrgSettings_then_LoadOrgSettings_round_trips", func(t *testing.T) {
		orgID := "contract-org-" + runID
		// Seed the org first: Postgres enforces org_settings.org_id → organisations(id).
		if err := b.SaveOrg(ctx, &interfaces.Organisation{
			ID: orgID, Name: "Contract Org", OwnerID: "contract-owner-" + runID,
		}); err != nil {
			t.Fatalf("seed org: %v", err)
		}
		settings := &interfaces.AppSettings{
			Version: 1,
			General: interfaces.GeneralSettings{CheckForUpdates: "monthly"},
		}
		if err := b.SaveOrgSettings(ctx, orgID, settings); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}
		got, err := b.LoadOrgSettings(ctx, orgID)
		if err != nil {
			t.Fatalf("LoadOrgSettings: %v", err)
		}
		if got.General.CheckForUpdates != "monthly" {
			t.Errorf("round-trip: want monthly, got %s", got.General.CheckForUpdates)
		}
	})

	t.Run("LoadUsersByIDs_resolves_present_and_skips_missing", func(t *testing.T) {
		u1 := &interfaces.User{ID: "contract-multi-1-" + runID, Email: "m1-" + runID + "@example.com", Password: "h"}
		u2 := &interfaces.User{ID: "contract-multi-2-" + runID, Email: "m2-" + runID + "@example.com", Password: "h"}
		if err := b.CreateUser(ctx, u1); err != nil {
			t.Fatalf("create u1: %v", err)
		}
		if err := b.CreateUser(ctx, u2); err != nil {
			t.Fatalf("create u2: %v", err)
		}

		// Includes a duplicate and a missing id; both must be handled cleanly.
		got, err := b.LoadUsersByIDs(ctx, []string{u1.ID, u2.ID, "missing-" + runID, u1.ID})
		if err != nil {
			t.Fatalf("LoadUsersByIDs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 resolved users, got %d", len(got))
		}
		if got[u1.ID] == nil || got[u1.ID].Email != u1.Email {
			t.Errorf("u1 not resolved correctly: %+v", got[u1.ID])
		}
		if got[u2.ID] == nil || got[u2.ID].Email != u2.Email {
			t.Errorf("u2 not resolved correctly: %+v", got[u2.ID])
		}
		if _, ok := got["missing-"+runID]; ok {
			t.Errorf("missing id should be absent from result map")
		}
	})

	t.Run("LoadUsersByIDs_empty_returns_empty_map", func(t *testing.T) {
		got, err := b.LoadUsersByIDs(ctx, nil)
		if err != nil {
			t.Fatalf("LoadUsersByIDs(nil): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %d entries", len(got))
		}
	})

	t.Run("ListFlows_MetadataOnly_omits_content_keeps_metadata", func(t *testing.T) {
		owner := "contract-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:       "contract-flow-meta-" + runID,
			Name:     "Meta Flow",
			Content:  json.RawMessage(`{"big":"` + strings.Repeat("x", 1000) + `"}`),
			Metadata: interfaces.FlowMetadata{BlockCount: 7, SubflowCount: 2},
			OwnerID:  owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		metaOnly, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, MetadataOnly: true})
		if err != nil {
			t.Fatalf("ListFlows meta-only: %v", err)
		}
		found := findFlow(metaOnly, flow.ID)
		if found == nil {
			t.Fatalf("flow %s not present in meta-only list", flow.ID)
		}
		if len(found.Content) != 0 {
			t.Errorf("MetadataOnly: expected empty content, got %d bytes", len(found.Content))
		}
		if found.Metadata.BlockCount != 7 {
			t.Errorf("MetadataOnly: metadata lost, BlockCount=%d want 7", found.Metadata.BlockCount)
		}

		full, err := b.ListFlows(ctx, interfaces.FlowFilter{UserID: owner})
		if err != nil {
			t.Fatalf("ListFlows full: %v", err)
		}
		foundFull := findFlow(full, flow.ID)
		if foundFull == nil {
			t.Fatalf("flow %s not present in full list", flow.ID)
		}
		if len(foundFull.Content) == 0 {
			t.Errorf("full list: expected content present, got empty")
		}
	})

	t.Run("LoadFlowHeader_returns_metadata_without_content", func(t *testing.T) {
		owner := "contract-hdr-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:       "contract-flow-hdr-" + runID,
			Name:     "Header Flow",
			Content:  json.RawMessage(`{"big":"` + strings.Repeat("y", 1000) + `"}`),
			Metadata: interfaces.FlowMetadata{BlockCount: 5, SubflowCount: 1},
			OwnerID:  owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		hdr, err := b.LoadFlowHeader(ctx, flow.ID)
		if err != nil {
			t.Fatalf("LoadFlowHeader: %v", err)
		}
		if len(hdr.Content) != 0 {
			t.Errorf("LoadFlowHeader: expected nil content, got %d bytes", len(hdr.Content))
		}
		if hdr.OwnerID != owner {
			t.Errorf("LoadFlowHeader: OwnerID=%q want %q", hdr.OwnerID, owner)
		}
		if hdr.Metadata.BlockCount != 5 {
			t.Errorf("LoadFlowHeader: metadata lost, BlockCount=%d want 5", hdr.Metadata.BlockCount)
		}

		// LoadFlow (with content) must still return the full payload.
		full, err := b.LoadFlow(ctx, flow.ID)
		if err != nil {
			t.Fatalf("LoadFlow: %v", err)
		}
		if len(full.Content) == 0 {
			t.Errorf("LoadFlow: expected content present, got empty")
		}

		if _, err := b.LoadFlowHeader(ctx, "no-such-flow-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadFlowHeader(missing): expected ErrNotFound, got %v", err)
		}
	})

	t.Run("ListFlows_unscoped_matches_nothing_AllFlows_enumerates", func(t *testing.T) {
		// Both real backends must agree: an unscoped filter (no UserID, no
		// OrganizationID, no AllFlows) is a defense-in-depth no-match, while
		// AllFlows enumerates every flow regardless of owner. The migrator
		// depends on the latter; the former stops a forgotten scope from
		// dumping the whole table. Assertions are written to be robust against
		// a shared/long-lived Postgres DB (no exact-count checks under AllFlows).
		owner := "contract-allflows-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:      "contract-flow-allflows-" + runID,
			Name:    "AllFlows Flow",
			Content: json.RawMessage(`{"k":"v"}`),
			OwnerID: owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// Unscoped: 1=0 / matchesFilter→false. Returns nothing on both backends
		// regardless of pre-existing rows, so an exact length check is safe.
		unscoped, err := b.ListFlows(ctx, interfaces.FlowFilter{})
		if err != nil {
			t.Fatalf("ListFlows unscoped: %v", err)
		}
		if len(unscoped) != 0 {
			t.Errorf("unscoped filter: expected 0 flows (guard), got %d", len(unscoped))
		}

		// AllFlows: the seeded flow must be present. Other rows may exist on a
		// shared DB, so assert presence rather than an exact count.
		all, err := b.ListFlows(ctx, interfaces.FlowFilter{AllFlows: true})
		if err != nil {
			t.Fatalf("ListFlows AllFlows: %v", err)
		}
		if findFlow(all, flow.ID) == nil {
			t.Errorf("AllFlows: seeded flow %s missing from enumeration", flow.ID)
		}
	})

	t.Run("SaveFlow_stale_version_returns_ErrVersionConflict", func(t *testing.T) {
		// OCC is strict on both backends: an update whose Version does not match
		// the current stored version fails — there is no "0 skips the check"
		// escape hatch. Callers that update must load the current version first.
		owner := "contract-occ-owner-" + runID
		flow := &interfaces.FlowDocument{
			ID:      "contract-flow-occ-" + runID,
			Name:    "OCC Flow",
			Content: json.RawMessage(`{"k":1}`),
			OwnerID: owner,
		}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow create: %v", err)
		}
		// Save again with the backend-assigned version: must succeed and bump.
		flow.Content = json.RawMessage(`{"k":2}`)
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow update (current version): %v", err)
		}

		stale := &interfaces.FlowDocument{
			ID:      flow.ID,
			Name:    "OCC Flow stale",
			Content: json.RawMessage(`{"k":3}`),
			OwnerID: owner,
			Version: flow.Version - 1,
		}
		if err := b.SaveFlow(ctx, stale); !errors.Is(err, interfaces.ErrVersionConflict) {
			t.Errorf("stale version: expected ErrVersionConflict, got %v", err)
		}
	})

	t.Run("ListFindingStatuses_empty_returns_non_nil_slice", func(t *testing.T) {
		got, err := b.ListFindingStatuses(ctx, "no-such-flow-"+runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Errorf("empty finding statuses: expected non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("empty finding statuses: expected length 0, got %d", len(got))
		}
	})

	t.Run("FindingStatus_upsert_list_delete_roundtrip", func(t *testing.T) {
		owner := "contract-triage-owner-" + runID
		flowID := "contract-flow-triage-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Triage Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		st := &interfaces.FindingStatus{
			FlowID: flowID, FindingKey: "hardcoded-credential:blk-1", RuleID: "hardcoded-credential",
			Status: "suppressed", Justification: "test secret", AssigneeID: "u1", UpdatedBy: owner,
		}
		if err := b.SetFindingStatus(ctx, st); err != nil {
			t.Fatalf("SetFindingStatus: %v", err)
		}

		got, err := b.ListFindingStatuses(ctx, flowID)
		if err != nil {
			t.Fatalf("ListFindingStatuses: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 status, got %d", len(got))
		}
		if got[0].Status != "suppressed" || got[0].Justification != "test secret" || got[0].AssigneeID != "u1" {
			t.Errorf("status round-trip mismatch: %+v", got[0])
		}
		if got[0].UpdatedAt.IsZero() {
			t.Errorf("expected UpdatedAt to be set by the backend")
		}

		// Upsert: same key updates in place rather than inserting a duplicate.
		st.Status = "resolved"
		if err := b.SetFindingStatus(ctx, st); err != nil {
			t.Fatalf("SetFindingStatus update: %v", err)
		}
		got, _ = b.ListFindingStatuses(ctx, flowID)
		if len(got) != 1 {
			t.Fatalf("upsert should not add a row; got %d", len(got))
		}
		if got[0].Status != "resolved" {
			t.Errorf("upsert: expected status=resolved, got %q", got[0].Status)
		}

		// Delete is idempotent: a second delete of a missing key is not an error.
		if err := b.DeleteFindingStatus(ctx, flowID, st.FindingKey); err != nil {
			t.Fatalf("DeleteFindingStatus: %v", err)
		}
		if err := b.DeleteFindingStatus(ctx, flowID, st.FindingKey); err != nil {
			t.Fatalf("DeleteFindingStatus (idempotent): %v", err)
		}
		got, _ = b.ListFindingStatuses(ctx, flowID)
		if len(got) != 0 {
			t.Errorf("expected 0 statuses after delete, got %d", len(got))
		}
	})

	t.Run("APIToken_create_lookup_list_delete", func(t *testing.T) {
		// A token belongs to a user; create the owner so the FK (Postgres) holds.
		owner := "contract-token-owner-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: owner, Email: "contract-token-" + runID + "@example.com", Password: "hash",
		}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		hash := "contract-token-hash-" + runID
		tok := &interfaces.APIToken{ID: "contract-tok-" + runID, UserID: owner, Name: "ci", TokenHash: hash}
		if err := b.CreateAPIToken(ctx, tok); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}

		// Lookup by hash is the auth path.
		got, err := b.GetAPITokenByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetAPITokenByHash: %v", err)
		}
		if got.UserID != owner || got.Name != "ci" {
			t.Errorf("token round-trip mismatch: %+v", got)
		}

		// Unknown hash ⇒ ErrNotFound (so a bad token is rejected, not matched).
		if _, err := b.GetAPITokenByHash(ctx, "no-such-hash-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing token hash: expected ErrNotFound, got %v", err)
		}

		list, err := b.ListAPITokens(ctx, owner)
		if err != nil {
			t.Fatalf("ListAPITokens: %v", err)
		}
		if len(list) != 1 || list[0].ID != tok.ID {
			t.Fatalf("expected the owner's single token, got %+v", list)
		}

		// A different user cannot delete it (scoped delete is a no-op).
		if err := b.DeleteAPIToken(ctx, "someone-else-"+runID, tok.ID); err != nil {
			t.Fatalf("DeleteAPIToken (wrong owner): %v", err)
		}
		if _, err := b.GetAPITokenByHash(ctx, hash); err != nil {
			t.Error("token must survive a delete attempt by a non-owner")
		}

		// Owner delete removes it; revoked ⇒ no longer found (auth rejects it).
		if err := b.DeleteAPIToken(ctx, owner, tok.ID); err != nil {
			t.Fatalf("DeleteAPIToken: %v", err)
		}
		if _, err := b.GetAPITokenByHash(ctx, hash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("deleted token: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("FlowBaseline_missing_then_set_clear", func(t *testing.T) {
		owner := "contract-baseline-owner-" + runID
		flowID := "contract-flow-baseline-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Baseline Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// Missing baseline: (nil, nil), not ErrNotFound.
		bl, err := b.GetFlowBaseline(ctx, flowID)
		if err != nil {
			t.Fatalf("GetFlowBaseline missing: %v", err)
		}
		if bl != nil {
			t.Errorf("expected nil baseline before set, got %+v", bl)
		}

		keys := []string{"r1:b1", "r2:b2"}
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: keys, CreatedBy: owner}); err != nil {
			t.Fatalf("SetFlowBaseline: %v", err)
		}
		bl, err = b.GetFlowBaseline(ctx, flowID)
		if err != nil {
			t.Fatalf("GetFlowBaseline: %v", err)
		}
		if bl == nil || len(bl.Keys) != 2 {
			t.Fatalf("expected baseline with 2 keys, got %+v", bl)
		}
		if bl.CreatedAt.IsZero() {
			t.Errorf("expected CreatedAt to be set by the backend")
		}

		// Replace (one baseline per flow): a second set overwrites, not appends.
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{"r3:b3"}, CreatedBy: owner}); err != nil {
			t.Fatalf("SetFlowBaseline replace: %v", err)
		}
		bl, _ = b.GetFlowBaseline(ctx, flowID)
		if bl == nil || len(bl.Keys) != 1 || bl.Keys[0] != "r3:b3" {
			t.Errorf("expected baseline replaced to single key r3:b3, got %+v", bl)
		}

		if err := b.ClearFlowBaseline(ctx, flowID); err != nil {
			t.Fatalf("ClearFlowBaseline: %v", err)
		}
		bl, _ = b.GetFlowBaseline(ctx, flowID)
		if bl != nil {
			t.Errorf("expected nil baseline after clear, got %+v", bl)
		}
		// Clear is idempotent.
		if err := b.ClearFlowBaseline(ctx, flowID); err != nil {
			t.Errorf("ClearFlowBaseline (idempotent): %v", err)
		}
	})

	// Governance alerts inbox: record → list (newest-first) → unread count →
	// mark-read → dismiss → clear. Mirrors the notifications-bell lifecycle.
	t.Run("GovernanceAlerts_record_list_read_dismiss_clear", func(t *testing.T) {
		owner := "contract-gov-owner-" + runID
		flowID := "contract-flow-gov-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Gov Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// Empty list returns a non-nil slice, never nil.
		got, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts empty: %v", err)
		}
		if got == nil {
			t.Fatal("empty list: expected non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("expected 0 alerts, got %d", len(got))
		}

		n, err := b.UnreadGovernanceAlertCount(ctx)
		if err != nil {
			t.Fatalf("UnreadGovernanceAlertCount empty: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 unread, got %d", n)
		}

		// Record two alerts (drift + regression). Timestamps ensure stable
		// newest-first ordering even if both run within the same millisecond.
		a1 := &interfaces.GovernanceAlert{
			ID: flowID + "|drift|e1w0", FlowID: flowID, FlowName: "Gov Flow",
			Type: "drift", Title: "New findings in Gov Flow", Severity: "error",
			NewErrors: 1, CreatedAt: time.Now().Add(-1 * time.Minute).UTC(),
		}
		a2 := &interfaces.GovernanceAlert{
			ID: flowID + "|health_regression|h80<90", FlowID: flowID, FlowName: "Gov Flow",
			Type: "health_regression", Title: "Health regressed in Gov Flow", Severity: "error",
			HealthScore: 80, PrevHealth: 90, CreatedAt: time.Now().UTC(),
		}
		if err := b.RecordGovernanceAlert(ctx, a1); err != nil {
			t.Fatalf("RecordGovernanceAlert a1: %v", err)
		}
		// Duplicate ID is a no-op (scanner retry safety).
		if err := b.RecordGovernanceAlert(ctx, a1); err != nil {
			t.Fatalf("RecordGovernanceAlert dup: %v", err)
		}
		if err := b.RecordGovernanceAlert(ctx, a2); err != nil {
			t.Fatalf("RecordGovernanceAlert a2: %v", err)
		}

		got, err = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 alerts, got %d", len(got))
		}
		// Newest-first: a2 (health, later timestamp) before a1 (drift).
		if got[0].ID != a2.ID {
			t.Errorf("expected newest-first (a2), got %q first", got[0].ID)
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != 2 {
			t.Errorf("expected 2 unread, got %d", n)
		}

		// Mark one read → unread drops to 1.
		if err := b.MarkGovernanceAlertRead(ctx, a1.ID); err != nil {
			t.Fatalf("MarkGovernanceAlertRead: %v", err)
		}
		// Idempotent.
		if err := b.MarkGovernanceAlertRead(ctx, a1.ID); err != nil {
			t.Errorf("MarkGovernanceAlertRead (idempotent): %v", err)
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != 1 {
			t.Errorf("expected 1 unread after read, got %d", n)
		}

		// Dismiss a2 → it's hidden from the default list, unread drops to 0.
		if err := b.DismissGovernanceAlert(ctx, a2.ID); err != nil {
			t.Fatalf("DismissGovernanceAlert: %v", err)
		}
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if len(got) != 1 || got[0].ID != a1.ID {
			t.Errorf("expected only the non-dismissed a1, got %+v", got)
		}
		// includeDismissed surfaces it again.
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{IncludeDismissed: true})
		if len(got) != 2 {
			t.Errorf("expected 2 alerts including dismissed, got %d", len(got))
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != 0 {
			t.Errorf("expected 0 unread after dismiss, got %d", n)
		}

		// Clear removes dismissed alerts permanently.
		if err := b.ClearGovernanceAlerts(ctx); err != nil {
			t.Fatalf("ClearGovernanceAlerts: %v", err)
		}
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{IncludeDismissed: true})
		if len(got) != 1 || got[0].ID != a1.ID {
			t.Errorf("expected only a1 after clear, got %+v", got)
		}

		// Mark-all-read clears remaining unread badge.
		if err := b.MarkAllGovernanceAlertsRead(ctx); err != nil {
			t.Fatalf("MarkAllGovernanceAlertsRead: %v", err)
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != 0 {
			t.Errorf("expected 0 unread after mark-all, got %d", n)
		}
	})
}
