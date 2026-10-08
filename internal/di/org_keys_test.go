package di

import (
	"errors"
	"testing"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

// notFoundKeys reports every key as absent the way both keystores do —
// with storage.ErrSecretNotFound — and one provider as broken.
type notFoundKeys struct{ testutil.FakeSecretStore }

var errKeysBroken = errors.New("keystore: open: message authentication failed")

func (k *notFoundKeys) Get(_, provider string) (string, error) {
	if provider == "broken" {
		return "", errKeysBroken
	}
	return "", storage.ErrSecretNotFound
}

// G7 plumbing: the factory sees a missing key as ("", nil) — so a plainly
// unconfigured provider is ErrKeyNotConfigured and ForOrg can reach the
// organisation's credential — while a real keystore failure still surfaces.
func TestMemberKeyLookup_MapsNotFoundOnly(t *testing.T) {
	get := memberKeyLookup(&notFoundKeys{})
	if v, err := get("u1", "claude"); v != "" || err != nil {
		t.Errorf("missing key = (%q, %v), want (\"\", nil)", v, err)
	}
	if _, err := get("u1", "broken"); !errors.Is(err, errKeysBroken) {
		t.Errorf("broken key err = %v, want the keystore's error", err)
	}

	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = orgKeys.SaveOrg("org-1", "azure-foundry", "doc", "admin")
	getOrg := orgKeyLookup(orgKeys)
	if v, err := getOrg("org-1", "azure-foundry"); v != "doc" || err != nil {
		t.Errorf("org key = (%q, %v)", v, err)
	}
	if v, err := getOrg("org-2", "azure-foundry"); v != "" || err != nil {
		t.Errorf("missing org key = (%q, %v), want (\"\", nil)", v, err)
	}
}
