package contract

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runCICredentialScenarios holds the organisations' CI credentials (ADR 0019) and the org-scoped flow read the CI
// webhook resolves its flow with. The webhook runs on the pool, outside any request's row-level security, so the
// organisation predicate of LoadFlowHeaderInOrg is the whole tenant boundary: a credential of one organisation must
// never reach another's flow, nor a personal one.
func runCICredentialScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	setup := func(t *testing.T, label string) (orgA, orgB, admin string) {
		t.Helper()
		admin = "contract-ci-" + label + "-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgA, orgB = "contract-ci-"+label+"-a-"+runID, "contract-ci-"+label+"-b-"+runID
		for _, id := range []string{orgA, orgB} {
			if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: id, Name: id, OwnerID: admin,
				Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
				t.Fatalf("SaveOrg %s: %v", id, err)
			}
			id := id
			t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), id) })
		}
		return orgA, orgB, admin
	}

	run("LoadFlowHeaderInOrg_reaches_only_the_organisations_flows", func(t *testing.T) {
		orgA, orgB, admin := setup(t, "scope")
		inA, inB, personal := "contract-ci-flow-a-"+runID, "contract-ci-flow-b-"+runID, "contract-ci-flow-p-"+runID
		for _, f := range []*interfaces.FlowDocument{
			{ID: inA, Name: "In A", OwnerID: admin, OrganizationID: orgA},
			{ID: inB, Name: "In B", OwnerID: admin, OrganizationID: orgB},
			{ID: personal, Name: "Personal", OwnerID: admin},
		} {
			f.Content = json.RawMessage(`{"name":"x"}`)
			if err := b.SaveFlow(ctx, f); err != nil {
				t.Fatalf("SaveFlow %s: %v", f.ID, err)
			}
			id := f.ID
			t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
		}

		h, err := b.LoadFlowHeaderInOrg(ctx, inA, orgA)
		if err != nil || h == nil {
			t.Fatalf("LoadFlowHeaderInOrg(own org) = %v, %v", h, err)
		}
		if h.ID != inA || h.OrganizationID != orgA || h.Name != "In A" || h.OwnerID != admin || len(h.Content) != 0 {
			t.Errorf("header = %+v; want the flow's header without its content", h)
		}
		plain, err := b.LoadFlowHeader(ctx, inA)
		if err != nil || plain.Version != h.Version {
			t.Errorf("LoadFlowHeader version %v (%v), scoped %d: the scoped header must carry the version the document cache checks", plain, err, h.Version)
		}
		for name, c := range map[string]struct{ id, org string }{
			"another organisation's flow": {inB, orgA},
			"a personal flow":             {personal, orgA},
			"a personal flow, no org":     {personal, ""},
			"a flow that does not exist":  {"contract-ci-missing-" + runID, orgA},
			"the right flow, no org":      {inA, ""},
		} {
			if h, err := b.LoadFlowHeaderInOrg(ctx, c.id, c.org); !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("%s: LoadFlowHeaderInOrg = %+v, %v; want ErrNotFound", name, h, err)
			}
		}
	})

	run("CICredentials_create_list_revoke_cap_and_touch", func(t *testing.T) {
		orgA, orgB, admin := setup(t, "creds")
		base := time.Now().UTC().Truncate(time.Second)
		cred := func(id, org string, created time.Time, expires *time.Time) *interfaces.CICredential {
			return &interfaces.CICredential{ID: "contract-ci-" + id + "-" + runID, OrgID: org, Name: id,
				Secret: "baki_ci_" + id + "_secret-" + runID, SecretHint: "hint", CreatedBy: admin, CreatedAt: created, ExpiresAt: expires}
		}
		first := cred("first", orgA, base.Add(-2*time.Hour), nil)
		if err := b.CreateCICredential(ctx, first, 2); errors.Is(err, interfaces.ErrSecretSealingUnavailable) {
			if _, gerr := b.GetCICredential(ctx, first.ID); !errors.Is(gerr, interfaces.ErrNotFound) {
				t.Errorf("a refused credential was stored: %v", gerr)
			}
			t.Skip("backend without field encryption refuses CI credentials, as it must")
		} else if err != nil {
			t.Fatalf("CreateCICredential: %v", err)
		}
		if err := b.CreateCICredential(ctx, cred("ghost", "contract-ci-no-org-"+runID, base, nil), 2); err == nil {
			t.Error("a credential of an organisation that does not exist was stored")
		}
		past := base.Add(-time.Hour)
		expired := cred("expired", orgA, base.Add(-3*time.Hour), &past)
		if err := b.CreateCICredential(ctx, expired, 2); err != nil {
			t.Fatalf("CreateCICredential(expired): %v", err)
		}
		future := base.Add(24 * time.Hour)
		second := cred("second", orgA, base.Add(-time.Hour), &future)
		if err := b.CreateCICredential(ctx, second, 2); err != nil {
			t.Fatalf("CreateCICredential(second): the expired one must not count toward the cap: %v", err)
		}
		if err := b.CreateCICredential(ctx, cred("third", orgA, base, nil), 2); !errors.Is(err, interfaces.ErrCICredentialLimit) {
			t.Errorf("a third active credential under a cap of 2: %v, want ErrCICredentialLimit", err)
		}
		if err := b.CreateCICredential(ctx, cred("other", orgB, base, nil), 2); err != nil {
			t.Errorf("another organisation's cap is its own: %v", err)
		}

		got, err := b.GetCICredential(ctx, second.ID)
		if err != nil {
			t.Fatalf("GetCICredential: %v", err)
		}
		if got.Secret != second.Secret || got.OrgID != orgA || got.Name != "second" || got.SecretHint != "hint" || got.CreatedBy != admin ||
			!got.CreatedAt.Equal(second.CreatedAt) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) || got.LastUsedAt != nil {
			t.Errorf("GetCICredential = %+v; want the stored credential with its secret", got)
		}
		if _, err := b.GetCICredential(ctx, "contract-ci-missing-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetCICredential(missing) = %v, want ErrNotFound", err)
		}

		list, err := b.ListCICredentials(ctx, orgA)
		if err != nil {
			t.Fatalf("ListCICredentials: %v", err)
		}
		if ids := ciIDs(list); !slices.Equal(ids, []string{second.ID, first.ID, expired.ID}) {
			t.Errorf("ListCICredentials = %v; want every credential of the org, newest first", ids)
		}
		for _, c := range list {
			if c.Secret != "" {
				t.Errorf("ListCICredentials returned %s's secret", c.ID)
			}
		}
		if empty, err := b.ListCICredentials(ctx, "contract-ci-nobody-"+runID); err != nil || empty == nil || len(empty) != 0 {
			t.Errorf("ListCICredentials(no credentials) = %v, %v; want an empty, non-nil list", empty, err)
		}

		used := base.Add(-time.Minute)
		if err := b.TouchCICredential(ctx, first.ID); err != nil {
			t.Fatalf("TouchCICredential: %v", err)
		}
		touched, _ := b.GetCICredential(ctx, first.ID)
		if touched.LastUsedAt == nil || touched.LastUsedAt.Before(used) {
			t.Fatalf("TouchCICredential did not record the use: %v", touched.LastUsedAt)
		}
		stamp := *touched.LastUsedAt
		if err := b.TouchCICredential(ctx, first.ID); err != nil {
			t.Fatalf("TouchCICredential again: %v", err)
		}
		if again, _ := b.GetCICredential(ctx, first.ID); again.LastUsedAt == nil || !again.LastUsedAt.Equal(stamp) {
			t.Errorf("a second use within five minutes rewrote last_used_at: %v, then %v", stamp, again.LastUsedAt)
		}
		if err := b.TouchCICredential(ctx, "contract-ci-missing-"+runID); err != nil {
			t.Errorf("TouchCICredential(missing) = %v, want nil", err)
		}

		active, err := b.ActiveCICredentials(ctx, orgA, 10)
		if err != nil {
			t.Fatalf("ActiveCICredentials: %v", err)
		}
		if ids := ciIDs(active); !slices.Equal(ids, []string{first.ID, second.ID}) {
			t.Errorf("ActiveCICredentials = %v; want the unexpired ones, the used one first", ids)
		}
		for _, c := range active {
			if c.Secret == "" {
				t.Errorf("ActiveCICredentials left out %s's secret", c.ID)
			}
		}
		if capped, _ := b.ActiveCICredentials(ctx, orgA, 1); len(capped) != 1 || capped[0].ID != first.ID {
			t.Errorf("ActiveCICredentials(limit 1) = %v", ciIDs(capped))
		}

		if err := b.DeleteCICredential(ctx, orgB, first.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("revoking another organisation's credential = %v, want ErrNotFound", err)
		}
		if _, err := b.GetCICredential(ctx, first.ID); err != nil {
			t.Errorf("the credential went with another organisation's revoke: %v", err)
		}
		if err := b.DeleteCICredential(ctx, orgA, first.ID); err != nil {
			t.Fatalf("DeleteCICredential: %v", err)
		}
		if _, err := b.GetCICredential(ctx, first.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a revoked credential is still found: %v", err)
		}
		if err := b.DeleteCICredential(ctx, orgA, first.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("revoking twice = %v, want ErrNotFound", err)
		}

		// Erasing the minter keeps the credential, without who minted it; deleting the organisation takes them all.
		if err := b.DeleteUser(ctx, admin); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if c, err := b.GetCICredential(ctx, second.ID); err != nil || c.CreatedBy != "" {
			t.Errorf("after erasing the minter: %+v, %v; want the credential kept with created_by cleared", c, err)
		}
		if err := b.DeleteOrg(ctx, orgA); err != nil {
			t.Fatalf("DeleteOrg: %v", err)
		}
		for _, id := range []string{second.ID, expired.ID} {
			if _, err := b.GetCICredential(ctx, id); !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("%s survived its organisation: %v", id, err)
			}
		}
	})
}

func ciIDs(list []*interfaces.CICredential) []string {
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.ID
	}
	return out
}
