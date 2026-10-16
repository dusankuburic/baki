package contract

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// seedOrg saves an organisation with admin as its one admin and deletes it when the test ends: the organisation's
// policies, channels, custom rules and knowledge base need it (the foreign keys of migration 0048).
func seedOrg(ctx context.Context, t *testing.T, b interfaces.StorageBackend, id, admin string) {
	t.Helper()
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: id, Name: id, OwnerID: admin,
		Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
		t.Fatalf("SaveOrg %s: %v", id, err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), id) })
}

// seedUser creates an account (the email is the id at example.com) and erases it when the test ends.
func seedUser(ctx context.Context, t *testing.T, b interfaces.StorageBackend, id string) {
	t.Helper()
	if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
		t.Fatalf("CreateUser %s: %v", id, err)
	}
	t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
}

// runIntegrityScenarios holds the integrity rules of migration 0048: an organisation's configuration and content, and
// a session, need their parent; a knowledge chunk stays in its document's organisation; one account per email
// whatever its case; one invite per token, and an expired invite no longer blocks a new one for its address; a policy
// is stored under an id.
func runIntegrityScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("Org_content_and_sessions_need_their_parent", func(t *testing.T) {
		admin := "contract-ig-admin-" + runID
		seedUser(ctx, t, b, admin)
		org, gone := "contract-ig-org-"+runID, "contract-ig-no-org-"+runID
		seedOrg(ctx, t, b, org, admin)
		rule := func(orgID, id string) *interfaces.OrgCustomRule {
			return &interfaces.OrgCustomRule{ID: id, OrgID: orgID, RuleID: "ig-rule", Enabled: true,
				Config: []byte(`{"kind":"block","schemaVersion":1,"id":"ig-rule","name":"ig-rule","severity":"warning","rawTypeMatch":"^SET$"}`)}
		}
		channel := func(orgID, id string) *interfaces.OrgChannel {
			return &interfaces.OrgChannel{ID: id, OrgID: orgID, Name: "ops", Kind: "webhook", URL: "https://hooks.example.com/ig", Enabled: true}
		}
		for _, tc := range []struct {
			what       string
			orphan, ok func() error
		}{
			{"a policy",
				func() error {
					return b.SavePolicy(ctx, &models.Policy{ID: "contract-ig-pol-" + runID, OrgID: gone, Name: "Gate"})
				},
				func() error {
					return b.SavePolicy(ctx, &models.Policy{ID: "contract-ig-pol-" + runID, OrgID: org, Name: "Gate"})
				}},
			{"a channel",
				func() error { return b.SaveOrgChannel(ctx, channel(gone, "contract-ig-ch-gone-"+runID)) },
				func() error { return b.SaveOrgChannel(ctx, channel(org, "contract-ig-ch-"+runID)) }},
			{"a custom rule",
				func() error { return b.SaveOrgCustomRule(ctx, rule(gone, "contract-ig-cr-gone-"+runID)) },
				func() error { return b.SaveOrgCustomRule(ctx, rule(org, "contract-ig-cr-"+runID)) }},
			{"a knowledge document",
				func() error {
					return b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: "contract-ig-doc-gone-" + runID, OrgID: gone, Filename: "kb.md"})
				},
				func() error {
					return b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: "contract-ig-doc-" + runID, OrgID: org, Filename: "kb.md"})
				}},
			{"a session",
				func() error {
					return b.StoreRefreshToken(ctx, "contract-ig-jti-gone-"+runID, "contract-ig-nobody-"+runID, time.Now().Add(time.Hour), "", "")
				},
				func() error {
					return b.StoreRefreshToken(ctx, "contract-ig-jti-"+runID, admin, time.Now().Add(time.Hour), "", "")
				}},
		} {
			// The organisation's configuration names the refusal ErrOrgNotFound, a 404 and not a 500 (local mode without
			// accounts checks no membership first). A knowledge document is written as a member, so on Postgres the
			// row-level policy may refuse it first.
			if err := tc.orphan(); err == nil {
				t.Errorf("%s whose parent does not exist was stored", tc.what)
			} else if tc.what != "a session" && tc.what != "a knowledge document" && !errors.Is(err, interfaces.ErrOrgNotFound) {
				t.Errorf("%s of an organisation that does not exist: %v, want ErrOrgNotFound", tc.what, err)
			}
			if err := tc.ok(); err != nil {
				t.Errorf("%s with its parent: %v", tc.what, err)
			}
		}
		if p, err := b.ListPolicies(ctx, gone); err != nil || len(p) != 0 {
			t.Errorf("the missing organisation's policies = %v, %v; want none", p, err)
		}
		if ch, err := b.ListOrgChannels(ctx, gone, false); err != nil || len(ch) != 0 {
			t.Errorf("the missing organisation's channels = %v, %v; want none", ch, err)
		}
		if r, err := b.ListOrgCustomRules(ctx, gone, false); err != nil || len(r) != 0 {
			t.Errorf("the missing organisation's rules = %v, %v; want none", r, err)
		}
		if d, err := b.ListKnowledgeDocuments(ctx, gone, 0, 0); err != nil || len(d) != 0 {
			t.Errorf("the missing organisation's documents = %v, %v; want none", d, err)
		}
		if ok, err := b.IsRefreshTokenValid(ctx, "contract-ig-jti-gone-"+runID); err != nil || ok {
			t.Errorf("the missing account's session: (%v, %v), want none", ok, err)
		}
	})

	run("Knowledge_chunks_stay_in_their_documents_organisation", func(t *testing.T) {
		admin := "contract-igk-admin-" + runID
		seedUser(ctx, t, b, admin)
		org, other := "contract-igk-org-"+runID, "contract-igk-other-"+runID
		seedOrg(ctx, t, b, org, admin)
		seedOrg(ctx, t, b, other, admin)
		doc := "contract-igk-doc-" + runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: doc, OrgID: org, Filename: "kb.md"}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		vec := []float32{1, 0, 0}
		// The caller belongs to both organisations: only the document decides.
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{{ID: doc + "-elsewhere", DocID: doc, OrgID: other, Content: "x", Embedding: vec}}); err == nil {
			t.Error("a chunk filed under another organisation than its document's was stored")
		}
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: doc + "-w", OrgID: org, Filename: "kbw.md"},
			[]interfaces.KnowledgeChunk{{ID: doc + "-w-elsewhere", DocID: doc + "-w", OrgID: other, Content: "x", Embedding: vec}}); err == nil {
			t.Error("a document written with a chunk of another organisation was stored")
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{
			{ID: doc + "-same", DocID: doc, OrgID: org, Content: "same", Embedding: vec},
			{ID: doc + "-blank", DocID: doc, Content: "blank", Embedding: vec},
		}); err != nil {
			t.Fatalf("chunks in their document's organisation, or naming none: %v", err)
		}
		if n, _, err := b.CountKnowledgeChunks(ctx, org); err != nil || n != 2 {
			t.Errorf("the organisation's chunks = %d, %v; want the two it was given", n, err)
		}
		if n, _, err := b.CountKnowledgeChunks(ctx, other); err != nil || n != 0 {
			t.Errorf("the other organisation's chunks = %d, %v; want none", n, err)
		}
		if docs, err := b.ListKnowledgeDocuments(ctx, org, 0, 0); err != nil || len(docs) != 1 || docs[0].ID != doc {
			t.Errorf("the organisation's documents = %+v, %v; want only %s (the refused write stored nothing)", docs, err, doc)
		}
	})

	run("Emails_are_unique_whatever_their_case", func(t *testing.T) {
		first := "contract-igm-a-" + runID
		email := "contract-igm-" + runID + "@example.com"
		if err := b.CreateUser(ctx, &interfaces.User{ID: first, Email: email, Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), first) })
		upper := "Contract-IGM-" + runID + "@Example.com"
		second := "contract-igm-b-" + runID
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), second) })
		if err := b.CreateUser(ctx, &interfaces.User{ID: second, Email: upper, Password: "hash"}); !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("CreateUser with the address in other case: %v, want ErrEmailExists", err)
		}
		if err := b.SaveUser(ctx, &interfaces.User{ID: second, Email: upper, Password: "hash"}); !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("SaveUser with the address in other case: %v, want ErrEmailExists", err)
		}
		link := &interfaces.IdentityLink{Provider: "oidc", Subject: "contract-igm-" + runID, UserID: second, Email: upper}
		if err := b.SaveUserWithIdentityLink(ctx, &interfaces.User{ID: second, Email: upper, Password: "hash"}, link); !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("SaveUserWithIdentityLink with the address in other case: %v, want ErrEmailExists", err)
		}
		if _, err := b.LoadUserByID(ctx, second); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the refused account: %v, want ErrNotFound", err)
		}
		// The holder of the address may change its case.
		if err := b.SaveUser(ctx, &interfaces.User{ID: first, Email: upper, Password: "hash"}); err != nil {
			t.Errorf("the account itself saved with its address in other case: %v", err)
		}
	})

	run("OrgInvites_token_unique_and_an_expired_invite_gives_way", func(t *testing.T) {
		admin := "contract-igi-admin-" + runID
		org := "contract-igi-org-" + runID
		seedOrg(ctx, t, b, org, admin)
		now := time.Now().UTC().Truncate(time.Second)
		invite := func(name, email string, expires time.Time) *interfaces.OrgInvite {
			return &interfaces.OrgInvite{ID: "contract-igi-" + name + "-" + runID, OrgID: org, Email: email, Role: auth.RoleMember,
				InvitedBy: admin, TokenHash: "contract-igi-hash-" + name + "-" + runID, ExpiresAt: expires}
		}
		email := "contract-igi-" + runID + "@example.com"
		stale := invite("stale", email, now.Add(-time.Hour))
		if err := b.SaveOrgInvite(ctx, stale); err != nil {
			t.Fatalf("SaveOrgInvite (expired): %v", err)
		}
		fresh := invite("fresh", email, now.Add(time.Hour))
		if err := b.SaveOrgInvite(ctx, fresh); err != nil {
			t.Fatalf("a new invite while the address's only pending one has expired: %v", err)
		}
		if _, err := b.GetOrgInvite(ctx, org, stale.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the expired invite after the new one: %v, want ErrNotFound (it gave way)", err)
		}
		if got, err := b.GetOrgInvite(ctx, org, fresh.ID); err != nil || got.TokenHash != fresh.TokenHash {
			t.Errorf("the new invite = %+v, %v", got, err)
		}
		// A live pending invite still blocks, and stays.
		again := invite("again", email, now.Add(time.Hour))
		if err := b.SaveOrgInvite(ctx, again); !errors.Is(err, interfaces.ErrOrgInviteExists) {
			t.Errorf("a second invite while one is pending: %v, want ErrOrgInviteExists", err)
		}
		if _, err := b.GetOrgInvite(ctx, org, fresh.ID); err != nil {
			t.Errorf("the pending invite after the refused one: %v", err)
		}
		// One invite per token, whatever the address.
		twin := invite("twin", "contract-igi-twin-"+runID+"@example.com", now.Add(time.Hour))
		twin.TokenHash = fresh.TokenHash
		if err := b.SaveOrgInvite(ctx, twin); !errors.Is(err, interfaces.ErrOrgInviteExists) {
			t.Errorf("an invite with a token already stored: %v, want ErrOrgInviteExists", err)
		}
		if got, err := b.GetOrgInviteByTokenHash(ctx, fresh.TokenHash); err != nil || got.ID != fresh.ID {
			t.Errorf("the token resolves to %+v, %v; want the first invite", got, err)
		}
	})

	run("SavePolicy_requires_an_id", func(t *testing.T) {
		org := "contract-igp-org-" + runID
		seedOrg(ctx, t, b, org, "contract-igp-admin-"+runID)
		if err := b.SavePolicy(ctx, &models.Policy{OrgID: org, Name: "No id"}); err == nil {
			t.Error("a policy without an id was stored")
		}
		if list, err := b.ListPolicies(ctx, org); err != nil || len(list) != 0 {
			t.Errorf("policies after the refused save = %+v, %v; want none", list, err)
		}
	})
}
