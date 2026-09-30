package database_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
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
	// The personal Azure registry, plus rows that are not the user's: another
	// user's personal registry and an org registry (org rows hold no user
	// ids and belong to the organisation).
	reg := database.NewPostgresAzureRegistry(b)
	mine, theirs := interfaces.AzureUserOwner(uid), interfaces.AzureUserOwner(other)
	for _, o := range []interfaces.AzureOwner{mine, theirs, interfaces.AzureOrgOwner(orgID)} {
		if _, err := reg.CreateConnection(ctx, o, interfaces.AzureConnectionRecord{ID: "default", Name: "my-res"}); err != nil {
			t.Fatalf("CreateConnection %s: %v", o, err)
		}
	}
	if err := reg.RecordConnectionTest(ctx, mine, "default", &interfaces.AzureTestResult{OK: true, LatencyMs: 90}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateModel(ctx, mine, interfaces.AzureModelRecord{
		ID: "docswriter", ConnectionID: "default", Kind: interfaces.AzureKindClaude, Deployment: "claude-sonnet-4-6",
		DisplayName: "Docs writer", Purpose: interfaces.AzurePurposeCustom, PurposeCustom: "Docs",
		Description: "Writes flow documentation", ContextWindow: 200_000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	var hookCalls []string
	b.SetAzureExportConfigSource(func(_ context.Context, userID string, ids []string) (map[string]interfaces.AzureConnectionPublicConfig, error) {
		hookCalls = append(hookCalls, userID+":"+strings.Join(ids, ","))
		return map[string]interfaces.AzureConnectionPublicConfig{
			"default": {Endpoint: "https://my-res.openai.azure.com", AuthMode: "entra", TenantID: "tenant", ClientID: "client"},
		}, nil
	})

	check := func(t *testing.T, path string, export *interfaces.UserDataExport) {
		t.Helper()
		if reg := export.AzureRegistry; reg == nil || len(reg.Connections) != 1 || len(reg.Models) != 1 {
			t.Errorf("%s: azure registry = %+v, want the one personal resource and model", path, export.AzureRegistry)
		} else {
			c, m := reg.Connections[0], reg.Models[0]
			if c.ID != "default" || c.Name != "my-res" || c.Config.Endpoint != "https://my-res.openai.azure.com" ||
				c.Config.AuthMode != "entra" || c.Config.ClientID != "client" || c.LastTest == nil || !c.LastTest.OK {
				t.Errorf("%s: exported resource = %+v", path, c)
			}
			if m.ID != "docswriter" || m.Kind != interfaces.AzureKindClaude || m.DisplayName != "Docs writer" ||
				m.PurposeCustom != "Docs" || m.Description == "" || m.ContextWindow != 200_000 || m.MaxOutputTokens != 8192 {
				t.Errorf("%s: exported model = %+v", path, m)
			}
		}
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
	// ...in the same order: the streamed writer mirrors the struct's field
	// order by hand, so a section added to one and not in the same place in
	// the other would drift.
	if bufOrder, streamOrder := topLevelKeys(t, bufJSON), topLevelKeys(t, buf.Bytes()); strings.Join(bufOrder, ",") != strings.Join(streamOrder, ",") {
		t.Errorf("top-level key order differs:\n buffered %v\n streamed %v", bufOrder, streamOrder)
	}
	if !strings.Contains(strings.Join(topLevelKeys(t, bufJSON), ","), "sessions,azureRegistry,exportedAt") {
		t.Errorf("the azure registry section must sit between sessions and exportedAt: %v", topLevelKeys(t, bufJSON))
	}
	// Both writers asked the hook for this user's resources, and only those.
	for _, call := range hookCalls {
		if call != uid+":default" {
			t.Errorf("config hook called with %q, want %q", call, uid+":default")
		}
	}
	if len(hookCalls) != 2 {
		t.Errorf("config hook calls = %d, want one per export", len(hookCalls))
	}
	// Nothing secret can be in the section: its types carry no secret field.
	if section, _ := json.Marshal(buffered.AzureRegistry); strings.Contains(string(section), "ecret") || strings.Contains(string(section), `"key"`) {
		t.Errorf("the azure registry section carries a secret-looking field: %s", section)
	}

	// A failing config source fails the export on both paths: a
	// data-subject export is complete or it is nothing.
	b.SetAzureExportConfigSource(func(context.Context, string, []string) (map[string]interfaces.AzureConnectionPublicConfig, error) {
		return nil, errors.New("keystore unavailable")
	})
	if _, err := b.ExportUserData(ctx, uid); err == nil {
		t.Error("ExportUserData succeeded without the resources' configuration")
	}
	if err := b.ExportUserDataTo(ctx, uid, &bytes.Buffer{}); err == nil {
		t.Error("ExportUserDataTo succeeded without the resources' configuration")
	}
	// Without a source the section still carries the registry metadata.
	b.SetAzureExportConfigSource(nil)
	if export, err := b.ExportUserData(ctx, uid); err != nil || export.AzureRegistry == nil ||
		len(export.AzureRegistry.Connections) != 1 || export.AzureRegistry.Connections[0].Config != (interfaces.AzureConnectionPublicConfig{}) {
		t.Errorf("export without a config source = (%+v, %v), want the metadata with an empty config", export, err)
	}
}

// topLevelKeys returns a JSON object's keys in document order.
func topLevelKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %v %v", tok, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("skip value of %v: %v", tok, err)
		}
	}
	return keys
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
