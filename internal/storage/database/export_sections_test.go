package database_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_ExportSections_BothPathsCarryEverySection pins the data-subject
// export contents on BOTH export paths. The sections below were first added to
// ExportUserData only, while the account-export handler always takes the
// streamed ExportUserDataTo path on Postgres — so users kept receiving a bundle
// without them, capped at 1000 audit events, and every test stayed green.
func TestPostgres_ExportSections_BothPathsCarryEverySection(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := itoa(time.Now().UnixNano())

	uid := "sections-user-" + run
	other := "sections-other-" + run
	for _, u := range []*interfaces.User{
		{ID: uid, Email: "sections-" + run + "@example.com", Password: "x"},
		{ID: other, Email: "sections-other-" + run + "@example.com", Password: "x"},
	} {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
	}
	ownFlow, otherFlow := "sections-flow-"+run, "sections-otherflow-"+run
	orgID := "sections-org-" + run
	t.Cleanup(func() {
		_ = b.DeleteFlow(ctx, ownFlow)
		_ = b.DeleteFlow(ctx, otherFlow)
		_ = b.DeleteOrg(ctx, orgID)
		cleanupUser(t, b, uid)
		cleanupUser(t, b, other)
	})

	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: ownFlow, Name: "own", OwnerID: uid, Content: []byte(`{}`)}); err != nil {
		t.Fatalf("SaveFlow own: %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: otherFlow, Name: "theirs", OwnerID: other, Content: []byte(`{}`)}); err != nil {
		t.Fatalf("SaveFlow other: %v", err)
	}
	if err := b.SaveConversation(ctx, ownFlow, "thread-1", []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "why is this slow?"}}); err != nil {
		t.Fatalf("SaveConversation own: %v", err)
	}
	// A conversation on someone else's flow is theirs, not the user's.
	if err := b.SaveConversation(ctx, otherFlow, "thread-x", []interfaces.ChatMessage{{ID: "m2", Role: "user", Content: "not yours"}}); err != nil {
		t.Fatalf("SaveConversation other: %v", err)
	}
	// A comment the user wrote on another user's flow is still their words.
	if err := b.AddFindingComment(ctx, &interfaces.FindingComment{ID: "sections-c-" + run, FlowID: otherFlow, FindingKey: "r:b", AuthorID: uid, Body: "looks intentional"}); err != nil {
		t.Fatalf("AddFindingComment: %v", err)
	}
	if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: ownFlow, FindingKey: "r:b", RuleID: "r", Status: "acknowledged", Justification: "vendor API", UpdatedBy: uid}); err != nil {
		t.Fatalf("SetFindingStatus: %v", err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Sections Org", OwnerID: uid, Members: []interfaces.OrgMember{{UserID: uid, Role: "admin"}}}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	if err := b.StoreRefreshToken(ctx, "sections-jti-"+run, uid, time.Now().Add(time.Hour), "test-agent", "127.0.0.1"); err != nil {
		t.Fatalf("StoreRefreshToken: %v", err)
	}

	check := func(t *testing.T, path string, export *interfaces.UserDataExport) {
		t.Helper()
		if len(export.Conversations) != 1 || export.Conversations[0].FlowID != ownFlow || len(export.Conversations[0].Messages) != 1 {
			t.Errorf("%s: conversations = %+v, want exactly the one on the owned flow", path, export.Conversations)
		}
		if len(export.Comments) != 1 || export.Comments[0].Body != "looks intentional" {
			t.Errorf("%s: comments = %+v, want the authored comment", path, export.Comments)
		}
		if len(export.TriageDecisions) != 1 || export.TriageDecisions[0].Justification != "vendor API" {
			t.Errorf("%s: triage = %+v, want the recorded decision", path, export.TriageDecisions)
		}
		if len(export.Organisations) != 1 || export.Organisations[0].ID != orgID {
			t.Errorf("%s: organisations = %+v, want the one membership", path, export.Organisations)
		}
		if len(export.Sessions) != 1 {
			t.Errorf("%s: sessions = %+v, want the one active session", path, export.Sessions)
		}
	}

	buffered, err := b.ExportUserData(ctx, uid)
	if err != nil {
		t.Fatalf("ExportUserData: %v", err)
	}
	check(t, "buffered", buffered)

	var buf bytes.Buffer
	if err := b.ExportUserDataTo(ctx, uid, &buf); err != nil {
		t.Fatalf("ExportUserDataTo: %v", err)
	}
	var streamed interfaces.UserDataExport
	if err := json.Unmarshal(buf.Bytes(), &streamed); err != nil {
		t.Fatalf("streamed export is not valid JSON: %v\n%s", err, buf.String())
	}
	check(t, "streamed", &streamed)

	// The two paths must produce the same top-level keys.
	var bufKeys, streamKeys map[string]json.RawMessage
	bufJSON, _ := json.Marshal(buffered)
	_ = json.Unmarshal(bufJSON, &bufKeys)
	_ = json.Unmarshal(buf.Bytes(), &streamKeys)
	for k := range bufKeys {
		if _, ok := streamKeys[k]; !ok {
			t.Errorf("streamed export is missing the %q section", k)
		}
	}
}

// TestPostgres_IdentityLinks_ListAndDeleteAreUserScoped covers the storage half
// of account linking: a user lists only their own links and can remove only
// their own link for a provider.
func TestPostgres_IdentityLinks_ListAndDeleteAreUserScoped(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := itoa(time.Now().UnixNano())

	alice, bob := "idl-alice-"+run, "idl-bob-"+run
	for _, u := range []*interfaces.User{
		{ID: alice, Email: "idl-alice-" + run + "@example.com", Password: "x"},
		{ID: bob, Email: "idl-bob-" + run + "@example.com", Password: "x"},
	} {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
	}
	t.Cleanup(func() { cleanupUser(t, b, alice); cleanupUser(t, b, bob) })

	for _, l := range []*interfaces.IdentityLink{
		{Provider: "entra", Subject: "sub-a-" + run, UserID: alice, Email: "a@corp.example"},
		{Provider: "entra", Subject: "sub-b-" + run, UserID: bob},
	} {
		if err := b.SaveIdentityLink(ctx, l); err != nil {
			t.Fatalf("SaveIdentityLink: %v", err)
		}
	}

	links, err := b.ListIdentityLinks(ctx, alice)
	if err != nil {
		t.Fatalf("ListIdentityLinks: %v", err)
	}
	if len(links) != 1 || links[0].UserID != alice || links[0].Email != "a@corp.example" {
		t.Fatalf("alice's links = %+v, want only her entra link", links)
	}

	// Alice unlinking "entra" must not touch Bob's entra link.
	if err := b.DeleteIdentityLink(ctx, alice, "entra"); err != nil {
		t.Fatalf("DeleteIdentityLink: %v", err)
	}
	if links, _ := b.ListIdentityLinks(ctx, alice); len(links) != 0 {
		t.Errorf("alice still has links after unlink: %+v", links)
	}
	if links, _ := b.ListIdentityLinks(ctx, bob); len(links) != 1 {
		t.Errorf("bob's link must survive alice's unlink, got %+v", links)
	}
	// Deleting an absent link is a no-op.
	if err := b.DeleteIdentityLink(ctx, alice, "entra"); err != nil {
		t.Errorf("second unlink should be a no-op, got %v", err)
	}
}
