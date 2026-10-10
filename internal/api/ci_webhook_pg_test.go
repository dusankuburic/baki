package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/parser"
)

// TestCIWebhook_Postgres runs organisation CI credentials end to end on Postgres: minted through the admin's request
// transaction (the org-member policy, the per-org lock; not idempotent, so no response holding the credential is
// stored), the secret sealed at rest, the webhook's pool reads scoped to the credential's organisation, revoked
// through the API. A fresh database migrated to this binary's schema.
func TestCIWebhook_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	if err := pg.EnableFieldEncryption([]byte("ci-webhook-test-field-key-0123456789")); err != nil {
		t.Fatal(err)
	}
	rt := newTestRouter(pg, true)

	for _, id := range []string{"ci-admin", "ci-other"} {
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	org, other := seedOrg(t, rt, "Acme", "ci-admin"), seedOrg(t, rt, "Other", "ci-other")
	mirrorOrg(t, rt, org)
	mirrorOrg(t, rt, other)
	for id, owner := range map[string][2]string{"ci-flow-acme": {"ci-admin", org}, "ci-flow-other": {"ci-other", other}} {
		doc, err := parser.ParseText(rawSampleFlow, id+".txt", int64(len(rawSampleFlow)))
		if err != nil {
			t.Fatal(err)
		}
		content, _ := json.Marshal(doc)
		if err := pg.SaveFlow(ctx, &storageif.FlowDocument{ID: id, Name: id, OwnerID: owner[0], OrganizationID: owner[1], Content: content, Source: rawSampleFlow}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
	}
	admin := sessionBearer(t, rt, "ci-admin", auth.RoleMember)

	mint := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/orgs/"+org+"/ci-credentials", strings.NewReader(`{"name":"pipeline"}`))
		req.Header.Set("Authorization", admin)
		req.Header.Set(idempotencyKeyHeader, key)
		return serveCI(rt, req)
	}
	first := mint("ci-1")
	checkStatus(t, first, http.StatusCreated)
	var created ciCredentialCreated
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil || created.Credential == "" {
		t.Fatalf("create answer %s: %v", first.Body, err)
	}
	// A retry with the same Idempotency-Key mints another credential: replaying the first would hand it out again.
	again := mint("ci-1")
	checkStatus(t, again, http.StatusCreated)
	var second ciCredentialCreated
	if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil || second.Credential == "" || second.ID == created.ID {
		t.Errorf("retry: %s, want another credential", again.Body)
	}
	var n int
	if err := pg.DB().QueryRowContext(ctx, `SELECT count(*) FROM org_ci_credentials WHERE org_id = $1`, org).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d credentials minted (%v), want 2", n, err)
	}
	if err := pg.DB().QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE idem_key = 'ci-1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d stored responses for the mint (%v), want none", n, err)
	}
	var secret string
	if err := pg.DB().QueryRowContext(ctx, `SELECT secret FROM org_ci_credentials WHERE id = $1`, created.ID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(secret, created.Credential[len(created.Credential)-20:]) || !strings.HasPrefix(secret, "enc:v1:") {
		t.Errorf("the credential column holds the credential in plaintext: %.40q", secret)
	}

	body := map[string]string{"flowId": "ci-flow-acme"}
	checkStatus(t, serveCI(rt, ciCall(t, created.Credential, created.ID, body)), http.StatusOK)
	checkStatus(t, serveCI(rt, ciCall(t, created.Credential, "", body)), http.StatusOK)
	if rr := serveCI(rt, ciCall(t, created.Credential, created.ID, map[string]string{"flowId": "ci-flow-other"})); rr.Code != http.StatusNotFound {
		t.Errorf("another organisation's flow: %d %s, want 404", rr.Code, rr.Body)
	}
	if rr := serveCI(rt, ciCall(t, created.Credential, "", map[string]string{"flowId": "ci-flow-other"})); rr.Code != http.StatusUnauthorized {
		t.Errorf("another organisation's flow without a key id: %d, want 401", rr.Code)
	}

	list := doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+org+"/ci-credentials", admin, nil)
	checkStatus(t, list, http.StatusOK)
	if strings.Contains(list.Body.String(), created.Credential) || !strings.Contains(list.Body.String(), created.ID) {
		t.Errorf("list = %s", list.Body)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+org+"/ci-credentials/"+created.ID, admin, nil), http.StatusNoContent)
	if rr := serveCI(rt, ciCall(t, created.Credential, created.ID, body)); rr.Code != http.StatusUnauthorized {
		t.Errorf("after the revoke: %d, want 401", rr.Code)
	}
}
