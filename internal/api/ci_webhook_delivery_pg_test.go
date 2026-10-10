package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/parser"
)

// TestCIWebhook_TokenAndDelivery_Postgres runs a personal CI token and the replay record end to end on Postgres:
// the token minted through the user's request transaction, the webhook's authorization and its delivery record on
// the pool (the table is visible there only), a replay refused, another user's flow a 404. A fresh database
// migrated to this binary's schema.
func TestCIWebhook_TokenAndDelivery_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt := newTestRouter(pg, true)

	for _, id := range []string{"ci-tok-alice", "ci-tok-bob"} {
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		doc, err := parser.ParseText(rawSampleFlow, id+".txt", int64(len(rawSampleFlow)))
		if err != nil {
			t.Fatal(err)
		}
		content, _ := json.Marshal(doc)
		if err := pg.SaveFlow(ctx, &storageif.FlowDocument{ID: id + "-flow", Name: id, OwnerID: id, Content: content, Source: rawSampleFlow}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
	}
	session := sessionBearer(t, rt, "ci-tok-alice", auth.RoleMember)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/tokens", session, map[string]any{"name": "ci", "scopes": []string{auth.ScopeCI}})
	checkStatus(t, rr, http.StatusOK)
	var created struct {
		Token string `json:"token"`
	}
	decodeJSON(t, rr, &created)
	bearer := "Bearer " + created.Token

	body := map[string]string{"flowId": "ci-tok-alice-flow", "deliveryId": "pg-delivery-0123456789abcdef"}
	checkStatus(t, serveCI(rt, tokenCI(t, bearer, body)), http.StatusOK)
	wantReplayed(t, "a replay on Postgres", serveCI(rt, tokenCI(t, bearer, body)))
	var n int
	if err := pg.DB().QueryRowContext(ctx, `SELECT count(*) FROM ci_webhook_deliveries WHERE delivery_id = $1 AND caller LIKE 'token:%'`, body["deliveryId"]).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d delivery record(s) (%v), want 1", n, err)
	}
	if rr := serveCI(rt, tokenCI(t, bearer, map[string]string{"flowId": "ci-tok-bob-flow"})); rr.Code != http.StatusNotFound {
		t.Errorf("another user's flow: %d %s, want 404", rr.Code, rr.Body.String())
	}
}
