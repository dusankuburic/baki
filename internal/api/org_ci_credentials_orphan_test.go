package api

import (
	"context"
	"net/http"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// A credential whose minter's account is gone without the erasure clearing created_by (erased by a release before
// the step, during a rollback) lists its minter as gone: createdBy and createdByName empty, never the raw user id.
func TestOrgCICredentials_ErasedMinterIsGone(t *testing.T) {
	f := newCIOrgFixture(t)
	const erased = "u-erased-before-the-step"
	if err := f.store.CreateCICredential(context.Background(), &storageif.CICredential{
		ID: "orphan", OrgID: f.org, Name: "left over", Secret: "s", SecretHint: "abcd", CreatedBy: erased,
	}, 10); err != nil {
		t.Fatal(err)
	}
	minted := f.mint(t, f.org, "current")

	rr := doRequestWithAuth(t, f.rt, http.MethodGet, "/api/orgs/"+f.org+"/ci-credentials", f.adminJWT, nil)
	checkStatus(t, rr, http.StatusOK)
	var list []ciCredentialOut
	decodeJSON(t, rr, &list)
	byID := map[string]ciCredentialOut{}
	for _, c := range list {
		byID[c.ID] = c
	}
	if c, ok := byID["orphan"]; !ok || c.CreatedBy != "" || c.CreatedByName != "" {
		t.Errorf("orphaned credential listed as %+v; want createdBy and createdByName empty", c)
	}
	if c := byID[minted.ID]; c.CreatedBy != f.admin || c.CreatedByName != "ci-admin@acme.io" {
		t.Errorf("current credential listed as %+v; want its minter", c)
	}
}
