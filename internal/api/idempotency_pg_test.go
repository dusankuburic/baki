package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/api/route"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestIdempotency_Postgres runs Idempotency-Key through the real request transaction (rlsMiddleware on a Postgres
// backend): the claim, the route's writes and the stored response commit together before the first response byte,
// and roll back together on an error response. The database is a fresh one migrated to this binary's schema.
func TestIdempotency_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt := newTestRouter(pg, true)

	newUser := func(t *testing.T) (id, bearer string) {
		t.Helper()
		id = "idem-" + uuid.NewString()
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		return id, jwtBearer(t, rt, id, id+"@example.com")
	}
	send := func(t *testing.T, bearer, method, path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", bearer)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set(idempotencyKeyHeader, key)
		}
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		return rr
	}
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pg.DB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	records := func(t *testing.T, userID, key string) int {
		return count(t, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND idem_key = $2`, userID, key)
	}

	// Test routes: each writes a flow through the request's transaction (named by the request), so whether the
	// write committed is visible afterwards.
	var slowStarted, slowRelease chan struct{}
	writeFlow := func(r *http.Request) error {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		return rt.security.Backend.SaveFlow(r.Context(), &storageif.FlowDocument{
			ID: "idem-flow-" + uuid.NewString(), Name: req.Name, OwnerID: rt.security.CallerID(r), Content: json.RawMessage(`{}`),
		})
	}
	mountTestRoutes(t, rt, func(rb *route.Builder) {
		rb.Post("/api/idem-pg/fail", func(w http.ResponseWriter, r *http.Request) {
			if err := writeFlow(r); err != nil {
				t.Errorf("write: %v", err)
			}
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "BOOM"})
		}, route.Idempotent())
		rb.Post("/api/idem-pg/slow", func(w http.ResponseWriter, r *http.Request) {
			if err := writeFlow(r); err != nil {
				t.Errorf("write: %v", err)
			}
			close(slowStarted)
			<-slowRelease
			writeJSONStatus(w, http.StatusCreated, map[string]any{"at": time.Now().UnixNano()})
		}, route.Idempotent())
		rb.Post("/api/idem-pg/big", func(w http.ResponseWriter, r *http.Request) {
			if err := writeFlow(r); err != nil {
				t.Errorf("write: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Repeat("y", maxStoredResponseBytes+10)))
		}, route.Idempotent())
	})

	t.Run("create, then replay without running again", func(t *testing.T) {
		user, bearer := newUser(t)
		body := `{"name":"Replayed flow","content":{}}`
		first := send(t, bearer, http.MethodPost, "/api/library", "lib-1", body)
		if first.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", first.Code, first.Body)
		}
		for range 2 {
			again := send(t, bearer, http.MethodPost, "/api/library", "lib-1", body)
			if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() || again.Header().Get(idempotentReplayedHeader) != "true" {
				t.Fatalf("retry: %d replayed=%q %s, want the first response replayed", again.Code, again.Header().Get(idempotentReplayedHeader), again.Body)
			}
		}
		if n := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1`, user); n != 1 {
			t.Errorf("%d flows created, want 1", n)
		}
		if n := count(t, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND idem_key = 'lib-1' AND status = 201`, user); n != 1 {
			t.Errorf("%d committed 201 records, want 1", n)
		}

		// Another body under the key is refused, and creates nothing.
		rr := send(t, bearer, http.MethodPost, "/api/library", "lib-1", `{"name":"Another flow"}`)
		if rr.Code != http.StatusUnprocessableEntity || errorCode(t, rr) != codeIdempotencyKeyReused {
			t.Errorf("another body: %d %s, want 422 %s", rr.Code, rr.Body, codeIdempotencyKeyReused)
		}
		if n := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1`, user); n != 1 {
			t.Errorf("%d flows after the refused reuse, want 1", n)
		}
	})

	t.Run("an error response leaves no record and no write", func(t *testing.T) {
		user, bearer := newUser(t)
		if rr := send(t, bearer, http.MethodPost, "/api/library", "lib-bad", `{"description":"no name"}`); rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid create: %d %s", rr.Code, rr.Body)
		}
		if n := records(t, user, "lib-bad"); n != 0 {
			t.Errorf("a 400 left %d records", n)
		}
		rr := send(t, bearer, http.MethodPost, "/api/library", "lib-bad", `{"name":"Fixed flow"}`)
		if rr.Code != http.StatusCreated || rr.Header().Get(idempotentReplayedHeader) != "" {
			t.Errorf("the corrected retry: %d %s, want a fresh 201", rr.Code, rr.Body)
		}

		if rr := send(t, bearer, http.MethodPost, "/api/idem-pg/fail", "fail-1", `{"name":"rolled back"}`); rr.Code != http.StatusInternalServerError {
			t.Fatalf("failing route: %d %s", rr.Code, rr.Body)
		}
		if n := records(t, user, "fail-1"); n != 0 {
			t.Errorf("a 500 left %d records", n)
		}
		if n := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1 AND name = 'rolled back'`, user); n != 0 {
			t.Errorf("the failed request's write survived: %d flows", n)
		}
	})

	t.Run("a concurrent duplicate is refused at once, then replays", func(t *testing.T) {
		user, bearer := newUser(t)
		slowStarted, slowRelease = make(chan struct{}), make(chan struct{})
		var first *httptest.ResponseRecorder
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			first = send(t, bearer, http.MethodPost, "/api/idem-pg/slow", "slow-1", `{"name":"slow"}`)
		}()
		<-slowStarted

		start := time.Now()
		dup := send(t, bearer, http.MethodPost, "/api/idem-pg/slow", "slow-1", `{"name":"slow"}`)
		if dup.Code != http.StatusConflict || errorCode(t, dup) != codeIdempotencyInProgress || dup.Header().Get("Retry-After") != "1" {
			t.Errorf("duplicate while the first runs: %d %s, want 409 %s", dup.Code, dup.Body, codeIdempotencyInProgress)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("the duplicate waited %v on the first request's transaction", waited)
		}
		close(slowRelease)
		wg.Wait()
		if first.Code != http.StatusCreated {
			t.Fatalf("first: %d %s", first.Code, first.Body)
		}
		again := send(t, bearer, http.MethodPost, "/api/idem-pg/slow", "slow-1", `{"name":"slow"}`)
		if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() || again.Header().Get(idempotentReplayedHeader) != "true" {
			t.Errorf("retry after the first committed: %d %s, want the replay", again.Code, again.Body)
		}
		if n := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1 AND name = 'slow'`, user); n != 1 {
			t.Errorf("%d flows written, want 1", n)
		}
	})

	t.Run("simultaneous retries create one flow", func(t *testing.T) {
		user, bearer := newUser(t)
		const n = 6
		statuses := make([]*httptest.ResponseRecorder, n)
		var wg sync.WaitGroup
		var ready sync.WaitGroup
		ready.Add(n)
		gate := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-gate
				statuses[i] = send(t, bearer, http.MethodPost, "/api/library", "race-1", `{"name":"Raced flow"}`)
			}()
		}
		ready.Wait()
		close(gate)
		wg.Wait()
		ran := 0
		for _, rr := range statuses {
			switch {
			case rr.Code == http.StatusCreated && rr.Header().Get(idempotentReplayedHeader) == "":
				ran++
			case rr.Code == http.StatusCreated, rr.Code == http.StatusConflict:
			default:
				t.Errorf("unexpected answer %d %s", rr.Code, rr.Body)
			}
		}
		if ran != 1 {
			t.Errorf("%d requests ran the create, want exactly 1", ran)
		}
		if got := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1`, user); got != 1 {
			t.Errorf("%d flows created, want 1", got)
		}
	})

	t.Run("a response over the cap replays its status only", func(t *testing.T) {
		user, bearer := newUser(t)
		first := send(t, bearer, http.MethodPost, "/api/idem-pg/big", "big-1", `{"name":"big"}`)
		if first.Code != http.StatusCreated || first.Body.Len() != maxStoredResponseBytes+10 {
			t.Fatalf("first: %d, %d bytes", first.Code, first.Body.Len())
		}
		if n := count(t, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND idem_key = 'big-1' AND status = 201 AND body IS NULL`, user); n != 1 {
			t.Errorf("%d status-only records, want 1", n)
		}
		again := send(t, bearer, http.MethodPost, "/api/idem-pg/big", "big-1", `{"name":"big"}`)
		if again.Code != http.StatusCreated || again.Body.Len() != 0 || again.Header().Get(idempotentReplayedHeader) != "true" {
			t.Errorf("replay: %d, %d bytes, want 201 without a body", again.Code, again.Body.Len())
		}
		if n := count(t, `SELECT count(*) FROM flows WHERE owner_id = $1 AND name = 'big'`, user); n != 1 {
			t.Errorf("%d flows written, want 1", n)
		}
	})

	t.Run("a new API token replays the same token, sealed at rest", func(t *testing.T) {
		if err := pg.EnableFieldEncryption([]byte("idempotency-test-field-key-0123456789")); err != nil {
			t.Fatal(err)
		}
		user, bearer := newUser(t)
		first := send(t, bearer, http.MethodPost, "/api/auth/tokens", "pat-1", `{"name":"ci"}`)
		if first.Code != http.StatusOK {
			t.Fatalf("create token: %d %s", first.Code, first.Body)
		}
		var tok struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &tok); err != nil || tok.Token == "" {
			t.Fatalf("token response %s: %v", first.Body, err)
		}
		again := send(t, bearer, http.MethodPost, "/api/auth/tokens", "pat-1", `{"name":"ci"}`)
		if again.Code != http.StatusOK || again.Body.String() != first.Body.String() {
			t.Errorf("retry: %d %s, want the same token back", again.Code, again.Body)
		}
		if n := count(t, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, user); n != 1 {
			t.Errorf("%d tokens minted, want 1", n)
		}
		var raw []byte
		if err := pg.DB().QueryRowContext(ctx, `SELECT body FROM idempotency_keys WHERE user_id = $1 AND idem_key = 'pat-1'`, user).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), tok.Token) || !strings.HasPrefix(string(raw), "enc:v1:") {
			t.Errorf("the stored response holds the token in plaintext: %.40q", raw)
		}
	})

	t.Run("an account's erasure removes its records", func(t *testing.T) {
		user, bearer := newUser(t)
		if rr := send(t, bearer, http.MethodPost, "/api/library", "erase-1", `{"name":"to erase"}`); rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body)
		}
		if err := pg.DeleteUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if n := records(t, user, "erase-1"); n != 0 {
			t.Errorf("%d records survived the erasure", n)
		}
	})

	t.Run("local mode ignores the header", func(t *testing.T) {
		local := newTestRouter(pg, false)
		var hits atomic.Int32
		mountTestRoutes(t, local, func(rb *route.Builder) {
			rb.Post("/api/idem-pg/local", func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusCreated)
			}, route.Idempotent())
		})
		for i := range 2 {
			req := httptest.NewRequest(http.MethodPost, "/api/idem-pg/local", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set(idempotencyKeyHeader, "local-1")
			rr := httptest.NewRecorder()
			local.ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated || rr.Header().Get(idempotentReplayedHeader) != "" {
				t.Errorf("request %d: %d replayed=%q", i, rr.Code, rr.Header().Get(idempotentReplayedHeader))
			}
		}
		if n := hits.Load(); n != 2 {
			t.Errorf("the route ran %d times, want 2: without accounts there is no request transaction to keep a key in", n)
		}
		if n := count(t, `SELECT count(*) FROM idempotency_keys WHERE idem_key = 'local-1'`); n != 0 {
			t.Errorf("local mode stored %d records", n)
		}
	})

	t.Run("expired records are purged by the retention sweep", func(t *testing.T) {
		user, bearer := newUser(t)
		for _, key := range []string{"old-1", "fresh-1"} {
			if rr := send(t, bearer, http.MethodPost, "/api/library", key, fmt.Sprintf(`{"name":%q}`, key)); rr.Code != http.StatusCreated {
				t.Fatalf("create %s: %d %s", key, rr.Code, rr.Body)
			}
		}
		if _, err := pg.DB().ExecContext(ctx, `UPDATE idempotency_keys SET expires_at = NOW() - INTERVAL '1 minute' WHERE user_id = $1 AND idem_key = 'old-1'`, user); err != nil {
			t.Fatal(err)
		}
		res, err := pg.PurgeExpiredData(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.IdempotencyKeys != 1 || records(t, user, "old-1") != 0 || records(t, user, "fresh-1") != 1 {
			t.Errorf("purge removed %d; old %d fresh %d records left, want 1 removed, the fresh one kept", res.IdempotencyKeys, records(t, user, "old-1"), records(t, user, "fresh-1"))
		}
		// The expired key is free again: a retry with it now creates.
		if rr := send(t, bearer, http.MethodPost, "/api/library", "old-1", `{"name":"old-1"}`); rr.Code != http.StatusCreated || rr.Header().Get(idempotentReplayedHeader) != "" {
			t.Errorf("the expired key: %d replayed=%q, want a fresh create", rr.Code, rr.Header().Get(idempotentReplayedHeader))
		}
	})
}
