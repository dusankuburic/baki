package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Lifecycle_ExportStreamedEqualsBuffered seeds a user with a row in every section and compares the
// streamed bundle with the buffered one byte for byte (the exportedAt stamps aside), both read inside the user's
// row-level-security scope as the export route does. It also pins the "own data only" contract: the owned flows
// and their chat, not a flow shared with the user; the user's own membership, not the other members; and the
// comments the user wrote, including one on a flow the user can no longer open.
func TestPostgres_Lifecycle_ExportStreamedEqualsBuffered(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	run := fmt.Sprintf("%x", time.Now().UnixNano())

	u, v := "export-u-"+run, "export-v-"+run
	f.ok("SaveUser u", b.SaveUser(ctx, &interfaces.User{ID: u, Email: u + "@example.com", Password: "x", DisplayName: "Ana <Export> & co"}))
	f.ok("SaveUser v", b.SaveUser(ctx, &interfaces.User{ID: v, Email: v + "@example.com", Password: "x"}))
	org := "export-org-" + run
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Export Org", OwnerID: u,
		Members: []interfaces.OrgMember{{UserID: u, Role: auth.RoleAdmin}, {UserID: v, Role: auth.RoleMember}}}))

	// More owned flows than one streamed page, so the page walk is part of the comparison.
	var owned []string
	for i := range exportFlowPageSize + 3 {
		id := fmt.Sprintf("export-own-%s-%03d", run, i)
		owned = append(owned, id)
		fl := &interfaces.FlowDocument{ID: id, Name: fmt.Sprintf("Flow <%d>", i), OwnerID: u, Content: json.RawMessage(`{"n":` + fmt.Sprint(i) + `}`), Tags: []string{"t"}}
		if i == 0 {
			fl.OrganizationID = org
		}
		f.ok("SaveFlow", b.SaveFlow(ctx, fl))
	}
	shared, lost := "export-shared-"+run, "export-lost-"+run
	f.ok("SaveFlow shared", b.SaveFlow(ctx, &interfaces.FlowDocument{ID: shared, Name: "theirs", OwnerID: v, Content: json.RawMessage(`{}`)}))
	f.ok("SaveFlow lost", b.SaveFlow(ctx, &interfaces.FlowDocument{ID: lost, Name: "lost", OwnerID: v, Content: json.RawMessage(`{}`)}))
	f.ok("AddCollaborator", b.AddCollaborator(ctx, shared, &interfaces.Collaborator{UserID: u, Permission: "edit"}))

	f.ok("SaveConversation own", b.SaveConversation(ctx, owned[1], "flow", []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "why?"}}))
	f.ok("SaveConversation shared", b.SaveConversation(ctx, shared, "flow", []interfaces.ChatMessage{{ID: "m2", Role: "user", Content: "theirs"}}))
	base := time.Now().UTC().Add(-time.Hour)
	for i, c := range []*interfaces.FindingComment{
		{ID: "c1-" + run, FlowID: shared, FindingKey: "r:1", AuthorID: u, Body: "mine on a shared flow"},
		{ID: "c2-" + run, FlowID: lost, FindingKey: "r:1", AuthorID: u, Body: "mine on a flow I lost"},
		{ID: "c3-" + run, FlowID: owned[2], FindingKey: "r:1", AuthorID: v, Body: "not mine"},
	} {
		c.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		f.ok("AddFindingComment", b.AddFindingComment(ctx, c))
	}
	f.ok("SetFindingStatus", b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: owned[2], FindingKey: "r:1", RuleID: "r", Status: "acknowledged", Justification: "fine", UpdatedBy: u}))
	f.ok("SaveUserSettings", b.SaveUserSettings(ctx, u, &interfaces.AppSettings{Version: 1}))
	f.ok("CreateAPIToken", b.CreateAPIToken(ctx, &interfaces.APIToken{ID: "tok-" + run, UserID: u, Name: "ci", TokenHash: "h-" + run, CreatedAt: base}))
	f.ok("StoreRefreshToken", b.StoreRefreshToken(ctx, "jti-"+run, u, time.Now().Add(time.Hour), "agent", "203.0.113.5"))
	for i := range 3 {
		f.ok("SaveAuditEvent", b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: fmt.Sprintf("ev-%d-%s", i, run), UserID: u, Action: "flow.save",
			ResourceType: "flow", ResourceID: owned[i], CreatedAt: base.Add(time.Duration(i) * time.Second)}))
	}
	_, err := NewPostgresAzureRegistry(b).CreateConnection(ctx, interfaces.AzureUserOwner(u), interfaces.AzureConnectionRecord{ID: "default", Name: "mine"})
	f.ok("CreateConnection", err)

	// As the export route does: a read scope for the user, fail-open.
	rctx, scope := WithLazyRLS(ctx, b, u, true)
	defer func() { _ = scope.Rollback() }()

	buffered, err := b.ExportUserData(rctx, u)
	if err != nil {
		t.Fatalf("ExportUserData: %v", err)
	}
	var streamed bytes.Buffer
	if err := b.ExportUserDataTo(rctx, u, &streamed); err != nil {
		t.Fatalf("ExportUserDataTo: %v", err)
	}
	bufferedJSON, err := json.Marshal(buffered)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := withoutExportedAt(t, bufferedJSON), withoutExportedAt(t, streamed.Bytes()); !bytes.Equal(a, b) {
		t.Errorf("the streamed bundle differs from the buffered one:\n buffered %s\n streamed %s", a, b)
	}

	var got interfaces.UserDataExport
	if err := json.Unmarshal(streamed.Bytes(), &got); err != nil {
		t.Fatalf("the streamed bundle is not JSON: %v", err)
	}
	var flowIDs []string
	for _, fl := range got.Flows {
		flowIDs = append(flowIDs, fl.ID)
		if fl.ID == owned[5] && string(fl.Content) != `{"n":5}` {
			t.Errorf("flow content = %s", fl.Content)
		}
	}
	if !slices.Equal(flowIDs, owned) {
		t.Errorf("flows = %d (%v...), want exactly the %d owned ones in id order", len(flowIDs), flowIDs[:min(3, len(flowIDs))], len(owned))
	}
	if len(got.Conversations) != 1 || got.Conversations[0].FlowID != owned[1] {
		t.Errorf("conversations = %+v, want only the owned flow's", got.Conversations)
	}
	var bodies []string
	for _, c := range got.Comments {
		bodies = append(bodies, c.Body)
	}
	if !slices.Equal(bodies, []string{"mine on a shared flow", "mine on a flow I lost"}) {
		t.Errorf("comments = %v, want the two the user wrote", bodies)
	}
	if len(got.TriageDecisions) != 1 || got.TriageDecisions[0].Justification != "fine" {
		t.Errorf("triage = %+v", got.TriageDecisions)
	}
	if len(got.Organisations) != 1 || len(got.Organisations[0].Members) != 1 || got.Organisations[0].Members[0].UserID != u {
		t.Errorf("organisations = %+v, want the one org with the user's own membership only", got.Organisations)
	}
	if len(got.Sessions) != 1 || len(got.APITokens) != 1 || len(got.AuditEvents) != 3 || got.Settings == nil ||
		got.AzureRegistry == nil || len(got.AzureRegistry.Connections) != 1 {
		t.Errorf("sessions %d, tokens %d, audit %d, settings %v, azure %+v", len(got.Sessions), len(got.APITokens), len(got.AuditEvents), got.Settings, got.AzureRegistry)
	}

	// A settings document that cannot be read fails both paths instead of shipping a bundle without it.
	f.exec(`UPDATE user_settings SET data = '{"general": 5}' WHERE user_id = $1`, u)
	if _, err := b.ExportUserData(ctx, u); err == nil {
		t.Error("ExportUserData succeeded with unreadable settings")
	}
	if err := b.ExportUserDataTo(ctx, u, &bytes.Buffer{}); err == nil {
		t.Error("ExportUserDataTo succeeded with unreadable settings")
	}
	// A missing user is ErrNotFound on both paths (the route answers 404).
	if _, err := b.ExportUserData(ctx, "export-nobody-"+run); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("ExportUserData of a missing user: %v, want ErrNotFound", err)
	}
	if err := b.ExportUserDataTo(ctx, "export-nobody-"+run, &bytes.Buffer{}); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("ExportUserDataTo of a missing user: %v, want ErrNotFound", err)
	}
}

// TestPostgres_Lifecycle_ExportOfAUserWithoutRows: an account with nothing else exports every section, the list
// sections as [] on both paths.
func TestPostgres_Lifecycle_ExportOfAUserWithoutRows(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	u := fmt.Sprintf("export-empty-%x", time.Now().UnixNano())
	if err := b.SaveUser(ctx, &interfaces.User{ID: u, Email: u + "@example.com", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	buffered, err := b.ExportUserData(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	if err := b.ExportUserDataTo(ctx, u, &streamed); err != nil {
		t.Fatal(err)
	}
	bufferedJSON, _ := json.Marshal(buffered)
	if a, b := withoutExportedAt(t, bufferedJSON), withoutExportedAt(t, streamed.Bytes()); !bytes.Equal(a, b) {
		t.Errorf("the streamed bundle differs from the buffered one:\n buffered %s\n streamed %s", a, b)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(streamed.Bytes(), &sections); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"flows", "auditEvents", "conversations", "comments", "triageDecisions", "organisations", "sessions"} {
		if string(sections[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, sections[key])
		}
	}
}

// withoutExportedAt returns the bundle with its exportedAt value blanked, the one field the two paths stamp apart.
func withoutExportedAt(t *testing.T, bundle []byte) []byte {
	t.Helper()
	const key = `,"exportedAt":"`
	i := bytes.LastIndex(bundle, []byte(key))
	if i < 0 {
		t.Fatalf("no exportedAt in %s", bundle)
	}
	end := bytes.IndexByte(bundle[i+len(key):], '"')
	if end < 0 {
		t.Fatalf("unterminated exportedAt in %s", bundle)
	}
	return slices.Concat(bundle[:i+len(key)], bundle[i+len(key)+end:])
}

// TestPostgres_Lifecycle_OwnedFlowWalkCompleteUnderConcurrentSave: a flow saved between two pages of the export's
// walk (the user in another tab) neither drops nor repeats a flow. Ordering on updated_at did: the saved flow jumped
// the page boundary and pushed the next row out of the walk, and the export's completeness checks then ran only
// over the flows that survived.
func TestPostgres_Lifecycle_OwnedFlowWalkCompleteUnderConcurrentSave(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	u := fmt.Sprintf("walk-%x", time.Now().UnixNano())
	if err := b.SaveUser(ctx, &interfaces.User{ID: u, Email: u + "@example.com", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	// A shared timestamp, so a page boundary sits inside a tied group (a bulk import, a restored backup).
	shared := time.Now().UTC().Truncate(time.Hour)
	var want []string
	for i := range 2*exportFlowPageSize + 5 {
		id := fmt.Sprintf("%s-%03d", u, i)
		want = append(want, id)
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: "f", OwnerID: u, Content: json.RawMessage(`{}`), UpdatedAt: shared}); err != nil {
			t.Fatal(err)
		}
	}
	x := &exportRun{userID: u}
	var seen []string
	saved := false
	if err := b.walkOwnedFlows(ctx, x, func(page []*interfaces.FlowDocument) error {
		for _, fl := range page {
			seen = append(seen, fl.ID)
		}
		if !saved {
			saved = true
			first, err := b.LoadFlow(ctx, want[0])
			if err != nil {
				return err
			}
			return b.SaveFlow(ctx, first)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !slices.Equal(seen, want) {
		t.Errorf("the walk saw %d flows, want each of the %d once in id order", len(seen), len(want))
	}
	if !slices.Equal(x.flowIDs, want) {
		t.Errorf("the walk recorded %d flow ids for the later sections, want %d", len(x.flowIDs), len(want))
	}
}
