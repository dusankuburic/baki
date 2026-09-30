package database_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// failingUpdateRegistry is the Postgres registry whose UpdateConnection
// fails while armed — a compare-and-swap race lost after the credential
// was saved.
type failingUpdateRegistry struct {
	interfaces.AzureRegistryStore
	fail atomic.Bool
}

func (r *failingUpdateRegistry) UpdateConnection(ctx context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureConnectionUpdate) (interfaces.AzureConnectionRecord, error) {
	if r.fail.Load() {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureRegistryConflict
	}
	return r.AzureRegistryStore.UpdateConnection(ctx, owner, id, u)
}

// TestAzureModelService_Postgres_CredentialChangeIsAllOrNothing (review S1)
// on the real stores: a rename the database's unique index refuses — here
// "Foİ" next to "Foi", one name under an en_US collation's lower() though
// Go's case folding tells them apart — is refused before the credential is
// saved; and a registry write that fails after the save puts the org
// credential back with its document, lender and compare-and-swap token.
func TestAzureModelService_Postgres_CredentialChangeIsAllOrNothing(t *testing.T) {
	b := openTestDB(t)
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{ExtraAllowedHosts: []string{"unit-test.invalid"}})
	t.Cleanup(func() { ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{}) })
	ks, err := b.NewEncryptedKeyStore([]byte("azure-service-integration-secret-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a1 := azureTestUser(t, b, "s1-a1")
	a2 := azureTestUser(t, b, "s1-a2")
	org := azureTestOrg(t, b, interfaces.OrgMember{UserID: a1, Role: auth.RoleAdmin}, interfaces.OrgMember{UserID: a2, Role: auth.RoleAdmin})
	reg := &failingUpdateRegistry{AzureRegistryStore: database.NewPostgresAzureRegistry(b)}
	svc := service.NewAzureModelService(service.AzureModelServiceDeps{
		Registry: reg, Secrets: ks, OrgKeys: ks, PerUserOwners: true, Orgs: b, Users: b,
	})
	cfg := func(key string) service.AzureConnectionConfigInput {
		return service.AzureConnectionConfigInput{Endpoint: "https://unit-test.invalid", Auth: ai.AzureFoundryAuth{Mode: ai.AzureAuthModeAPIKey, Key: key}}
	}
	s1 := service.AzureScope{Scope: a1, OrgID: org}
	s2 := service.AzureScope{Scope: a2, OrgID: org}
	if _, err := svc.CreateConnection(ctx, s1, service.AzureConnectionInput{Name: "Foi", Config: cfg("k-foi")}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateConnection(ctx, s1, service.AzureConnectionInput{Name: "Other", Config: cfg("k-a1")})
	if err != nil {
		t.Fatal(err)
	}
	key := ai.AzureConnectionKey(other.ID)
	before, _, err := ks.OrgKeyMeta(org, key)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		after, _, _ := ks.OrgKeyMeta(org, key)
		doc, _ := ks.GetOrg(org, key)
		if after.UpdatedBy != before.UpdatedBy || after.SecretSetBy != before.SecretSetBy || !after.UpdatedAt.Equal(before.UpdatedAt) ||
			!strings.Contains(doc, `"k-a1"`) {
			t.Errorf("%s: the org credential changed — meta %+v (was %+v), document holds k-a1: %v", what, after, before, strings.Contains(doc, `"k-a1"`))
		}
	}

	var folded bool
	if err := b.DB().QueryRowContext(ctx, `SELECT lower($1) = lower($2)`, "Foİ", "Foi").Scan(&folded); err != nil {
		t.Fatal(err)
	}
	newName, newCfg := "Foİ", cfg("k-a2-NEW")
	_, err = svc.UpdateConnection(ctx, s2, other.ID, service.AzureConnectionPatch{Name: &newName, Config: &newCfg, UpdatedAt: other.UpdatedAt, CredentialUpdatedAt: other.CredentialUpdatedAt})
	if folded {
		if _, code, _, _ := service.AzureRegistryHTTPError(err); code != service.AzureCodeDuplicateConnectionName {
			t.Errorf("rename onto a name the index folds together: %v (%s), want DUPLICATE_CONNECTION_NAME", err, code)
		}
		unchanged("the refused rename")
	} else {
		t.Logf("this database's collation keeps %q and %q apart (the rename went through: %v); the refusal is not exercised", "Foİ", "Foi", err)
		// Put the row and credential back as they were for the next part.
		other, _ = svc.GetConnection(ctx, s1, other.ID)
		before, _, _ = ks.OrgKeyMeta(org, key)
		if doc, _ := ks.GetOrg(org, key); !strings.Contains(doc, `"k-a1"`) {
			restore := cfg("k-a1")
			if _, err := svc.UpdateConnection(ctx, s1, other.ID, service.AzureConnectionPatch{Config: &restore, CredentialUpdatedAt: other.CredentialUpdatedAt}); err != nil {
				t.Fatal(err)
			}
			other, _ = svc.GetConnection(ctx, s1, other.ID)
			before, _, _ = ks.OrgKeyMeta(org, key)
		}
	}

	// The registry write fails after the save: the lender's credential and
	// its token go back, so the admin's retry with the token works.
	reg.fail.Store(true)
	_, err = svc.UpdateConnection(ctx, s2, other.ID, service.AzureConnectionPatch{Config: &newCfg, CredentialUpdatedAt: other.CredentialUpdatedAt})
	if _, code, _, _ := service.AzureRegistryHTTPError(err); code != service.AzureCodeConflict {
		t.Errorf("a lost race after the save: %v (%s), want CONFLICT", err, code)
	}
	if _, changed := service.AzureCredentialChanged(err); changed {
		t.Error("reported as changed, but the credential went back")
	}
	unchanged("the failed registry write")
	reg.fail.Store(false)
	if _, err := svc.UpdateConnection(ctx, s2, other.ID, service.AzureConnectionPatch{Config: &newCfg, CredentialUpdatedAt: other.CredentialUpdatedAt}); err != nil {
		t.Fatalf("the retry with the restored token: %v", err)
	}
	if meta, _, _ := ks.OrgKeyMeta(org, key); meta.SecretSetBy != a2 {
		t.Errorf("after the retry the lender is %q, want %q", meta.SecretSetBy, a2)
	}
}
