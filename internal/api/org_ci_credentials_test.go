package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// The organisation CI credential routes (ADR 0019), on the contract the settings UI reads: field names, statuses
// and codes.

func TestOrgCICredentials_CreateListRevoke(t *testing.T) {
	f := newCIOrgFixture(t)
	base := "/api/orgs/" + f.org + "/ci-credentials"

	rr := doRequestWithAuth(t, f.rt, http.MethodPost, base, f.adminJWT, map[string]any{"name": "  nightly  ", "expiresInDays": 30})
	checkStatus(t, rr, http.StatusCreated)
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "name", "secretHint", "createdBy", "createdByName", "createdAt", "expiresAt", "lastUsedAt", "credential"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("create answer lacks %q: %s", key, rr.Body.String())
		}
	}
	credential, _ := raw["credential"].(string)
	id, _ := raw["id"].(string)
	if !strings.HasPrefix(credential, service.CICredentialPrefix+id+"_") || service.CIKeyID(credential) != id {
		t.Errorf("credential %q does not carry its key id %q", credential, id)
	}
	if raw["name"] != "nightly" || raw["secretHint"] != credential[len(credential)-4:] || raw["createdBy"] != f.admin ||
		raw["createdByName"] != "ci-admin@acme.io" || raw["lastUsedAt"] != nil {
		t.Errorf("create answer = %s", rr.Body.String())
	}
	created, _ := time.Parse(time.RFC3339Nano, raw["createdAt"].(string))
	expires, _ := time.Parse(time.RFC3339Nano, raw["expiresAt"].(string))
	if expires.Sub(created) != 30*24*time.Hour {
		t.Errorf("expiresAt %v is not 30 days after createdAt %v", expires, created)
	}

	never := doRequestWithAuth(t, f.rt, http.MethodPost, base, f.adminJWT, map[string]any{"name": "forever"})
	checkStatus(t, never, http.StatusCreated)
	if !strings.Contains(never.Body.String(), `"expiresAt":null`) {
		t.Errorf("no expiry requested: %s", never.Body.String())
	}

	rr = doRequestWithAuth(t, f.rt, http.MethodGet, base, f.adminJWT, nil)
	checkStatus(t, rr, http.StatusOK)
	if strings.Contains(rr.Body.String(), credential) || strings.Contains(rr.Body.String(), `"credential"`) || strings.Contains(rr.Body.String(), "secret\"") {
		t.Fatalf("the list exposes a secret: %s", rr.Body.String())
	}
	var list []ciCredentialOut
	decodeJSON(t, rr, &list)
	if len(list) != 2 || list[0].CreatedAt.Before(list[1].CreatedAt) {
		t.Fatalf("list = %+v; want both, newest first", list)
	}
	listed := map[string]ciCredentialOut{list[0].ID: list[0], list[1].ID: list[1]}
	if c := listed[id]; c.Name != "nightly" || c.SecretHint != credential[len(credential)-4:] || c.ExpiresAt == nil {
		t.Errorf("listed %+v; want the minted credential's metadata", c)
	}

	// Revoke: 204, then the same id is unknown; another organisation's id is not this one's to revoke.
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, base+"/"+id, f.adminJWT, nil), http.StatusNoContent)
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, base+"/"+id, f.adminJWT, nil), http.StatusNotFound)
	addOrgMember(t, f.rt, f.other, f.admin, auth.RoleAdmin)
	theirs := f.mint(t, f.other, "theirs")
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, base+"/"+theirs.ID, f.adminJWT, nil), http.StatusNotFound)
	if c, err := f.store.GetCICredential(context.Background(), theirs.ID); err != nil || c == nil {
		t.Errorf("another organisation's credential went: %v", err)
	}

	// Audit: who minted and revoked what, never the secret.
	deadline := time.Now().Add(5 * time.Second)
	var creates, revokes int
	for time.Now().Before(deadline) {
		creates, revokes = 0, 0
		for _, ev := range f.store.SavedAuditEvents() {
			for _, v := range ev.Meta {
				if strings.Contains(v, credential) || strings.Contains(v, credential[len(service.CICredentialPrefix)+17:]) {
					t.Fatalf("audit row %s carries the secret", ev.Action)
				}
			}
			switch {
			case ev.Action == "org.ci_credential_create" && ev.OrgID == f.org:
				creates++
			case ev.Action == "org.ci_credential_revoke" && ev.OrgID == f.org && ev.Meta["keyId"] == id:
				revokes++
			}
		}
		if creates == 2 && revokes == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if creates != 2 || revokes != 1 {
		t.Errorf("audit: %d creates, %d revokes; want 2 and 1", creates, revokes)
	}
}

func TestOrgCICredentials_Validation(t *testing.T) {
	f := newCIOrgFixture(t)
	base := "/api/orgs/" + f.org + "/ci-credentials"
	for name, body := range map[string]any{
		"no name":            map[string]any{"expiresInDays": 5},
		"blank name":         map[string]any{"name": "   "},
		"long name":          map[string]any{"name": strings.Repeat("n", 101)},
		"expiry past a year": map[string]any{"name": "x", "expiresInDays": 366},
		"negative expiry":    map[string]any{"name": "x", "expiresInDays": -1},
		"unknown field":      map[string]any{"name": "x", "scopes": []string{"read"}},
		"string expiry":      map[string]any{"name": "x", "expiresInDays": "30"},
	} {
		if rr := doRequestWithAuth(t, f.rt, http.MethodPost, base, f.adminJWT, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
		}
	}
	for _, days := range []any{nil, 0, 1, 365} {
		rr := doRequestWithAuth(t, f.rt, http.MethodPost, base, f.adminJWT, map[string]any{"name": "ok", "expiresInDays": days})
		checkStatus(t, rr, http.StatusCreated)
	}
}

func TestOrgCICredentials_LimitAndEncryption(t *testing.T) {
	f := newCIOrgFixture(t)
	base := "/api/orgs/" + f.org + "/ci-credentials"
	for range service.MaxActiveCICredentials {
		f.mint(t, f.org, "runner")
	}
	rr := doRequestWithAuth(t, f.rt, http.MethodPost, base, f.adminJWT, map[string]any{"name": "eleventh"})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"code":"CI_CREDENTIAL_LIMIT"`) {
		t.Errorf("past the cap: %d %s, want 409 CI_CREDENTIAL_LIMIT", rr.Code, rr.Body.String())
	}

	f.store.FieldEncryptionOff = true
	addOrgMember(t, f.rt, f.other, f.admin, auth.RoleAdmin)
	rr = doRequestWithAuth(t, f.rt, http.MethodPost, "/api/orgs/"+f.other+"/ci-credentials", f.adminJWT, map[string]any{"name": "plain"})
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"code":"CI_CREDENTIALS_UNAVAILABLE"`) {
		t.Errorf("without field encryption: %d %s, want 503 CI_CREDENTIALS_UNAVAILABLE", rr.Code, rr.Body.String())
	}
}

// Org admins only, sessions only for the writes; organisations exist with accounts.
func TestOrgCICredentials_Access(t *testing.T) {
	f := newCIOrgFixture(t)
	base := "/api/orgs/" + f.org + "/ci-credentials"
	cred := f.mint(t, f.org, "pipeline")

	seedUserWithRole(t, f.rt, "u-ci-member", "member@acme.io", auth.RoleMember)
	addOrgMember(t, f.rt, f.org, "u-ci-member", auth.RoleMember)
	member := sessionBearer(t, f.rt, "u-ci-member", auth.RoleMember)
	globalAdmin := sessionBearer(t, f.rt, "u-ci-root", auth.RoleAdmin)
	for name, bearer := range map[string]string{"org member": member, "global admin outside the org": globalAdmin, "anonymous": ""} {
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, base, nil},
			{http.MethodPost, base, map[string]any{"name": "x"}},
			{http.MethodDelete, base + "/" + cred.ID, nil},
		} {
			rr := doRequestWithAuth(t, f.rt, c.method, c.path, bearer, c.body)
			if want := map[bool]int{true: http.StatusUnauthorized, false: http.StatusForbidden}[bearer == ""]; rr.Code != want {
				t.Errorf("%s %s as %s: %d, want %d", c.method, c.path, name, rr.Code, want)
			}
		}
	}

	pat := mintPAT(t, f.rt, f.adminJWT, []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin})
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodGet, base, pat, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodPost, base, pat, map[string]any{"name": "x"}), http.StatusForbidden)
	checkStatus(t, doRequestWithAuth(t, f.rt, http.MethodDelete, base+"/"+cred.ID, pat, nil), http.StatusForbidden)

	// local+db without accounts: no organisations, no routes.
	local := newTestRouter(testutil.NewFakeBackend(), false)
	rr := doRequest(t, local, http.MethodGet, "/api/orgs/o1/ci-credentials", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("without accounts: %d %s, want 503", rr.Code, rr.Body.String())
	}
}
