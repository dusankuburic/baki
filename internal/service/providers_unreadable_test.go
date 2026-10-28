package service

// R-provider-18: a credential the keystore holds but cannot decrypt is "unreadable" on the catalogue and the
// listing, for the caller's own key and the organisation's shared one, and an unreadable own key is not passed
// over for the organisation's (a turn reports the keystore failure rather than fall through to it).

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

type decryptFailingKeys struct {
	*testutil.FakeSecretStore
	bad map[string]bool // scope + "/" + provider
}

func (s decryptFailingKeys) Get(scope, provider string) (string, error) {
	if s.bad[scope+"/"+provider] {
		return "", storage.ErrSecretDecryptFailed
	}
	return s.FakeSecretStore.Get(scope, provider)
}

type decryptFailingOrgKeys struct {
	*testutil.FakeOrgSecretStore
	bad map[string]bool // org + "/" + provider
}

func (s decryptFailingOrgKeys) GetOrg(orgID, provider string) (string, error) {
	if s.bad[orgID+"/"+provider] {
		return "", storage.ErrSecretDecryptFailed
	}
	return s.FakeOrgSecretStore.GetOrg(orgID, provider)
}

func TestProviderCatalog_CredentialStatuses(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = secrets.Save("u1", "openai", "")                   // a legacy empty row: no request can use it
	_ = secrets.Save("u1", "glm", "stored-but-unreadable") // made unreadable below
	_ = orgKeys.SaveOrg("org-1", "glm", "org-glm-key", "admin")
	_ = orgKeys.SaveOrg("org-1", "gemini", "rotated", "admin")
	_ = orgKeys.SaveOrg("org-1", "xai", "org-xai-key", "admin")
	svc := newOrgProviderService(t, secrets, orgKeys)
	svc.secrets = decryptFailingKeys{FakeSecretStore: secrets, bad: map[string]bool{"u1/glm": true}}
	svc.orgKeys = decryptFailingOrgKeys{FakeOrgSecretStore: orgKeys, bad: map[string]bool{"org-1/gemini": true}}

	entries, err := svc.ProviderCatalog(context.Background(), "u1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		configured bool
		own, org   CredentialStatus
	}{
		"openai": {false, CredentialMissing, CredentialMissing},
		// An unreadable own key is what a turn reports, even beside a usable org key.
		"glm":    {false, CredentialUnreadable, CredentialConfigured},
		"gemini": {false, CredentialMissing, CredentialUnreadable},
		"xai":    {true, CredentialMissing, CredentialConfigured},
	}
	for _, e := range entries {
		w, ok := want[e.ID]
		if !ok {
			continue
		}
		delete(want, e.ID)
		if e.Configured != w.configured || e.CredentialStatus != string(w.own) || e.OrgCredentialStatus != string(w.org) {
			t.Errorf("%s: configured=%v own=%q org=%q, want %v %q %q", e.ID, e.Configured, e.CredentialStatus, e.OrgCredentialStatus, w.configured, w.own, w.org)
		}
	}
	for id := range want {
		t.Errorf("%s missing from the catalogue", id)
	}

	// The listing agrees: glm is not listed on the org's key while the member's own is unreadable.
	ls, own, configured := keyByKey(svc, "u1", "org-1").resolveListing("glm")
	if configured || ls.shared || own != CredentialUnreadable {
		t.Errorf("resolveListing(glm) = %+v, %q, %v; want the member's unreadable key, not the org's", ls, own, configured)
	}
	if ls, _, configured := keyByKey(svc, "u1", "org-1").resolveListing("xai"); !configured || !ls.shared {
		t.Errorf("resolveListing(xai) = %+v, %v; want the org's shared key", ls, configured)
	}
}
