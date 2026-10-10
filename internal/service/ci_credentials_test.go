package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/hmacsig"
	"pad-analyzer/internal/platform/secret"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// ciFixture is a fake store with two organisations, a flow in each and a personal flow, and a CI service over it.
type ciFixture struct {
	t     *testing.T
	ctx   context.Context
	store *testutil.FakeBackend
	svc   *CIService
}

func newCIFixture(t *testing.T, opts CIOptions) *ciFixture {
	t.Helper()
	store := testutil.NewFakeBackend()
	ctx := context.Background()
	for _, org := range []string{"org-a", "org-b"} {
		if err := store.SaveOrg(ctx, &storageif.Organisation{ID: org, Name: org, OwnerID: "admin",
			Members: []storageif.OrgMember{{UserID: "admin", Role: auth.RoleAdmin}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*storageif.FlowDocument{
		{ID: "flow-a", Name: "A", OwnerID: "admin", OrganizationID: "org-a"},
		{ID: "flow-b", Name: "B", OwnerID: "admin", OrganizationID: "org-b"},
		{ID: "flow-personal", Name: "Mine", OwnerID: "admin"},
	} {
		f.Content = json.RawMessage(`{"name":"` + f.Name + `"}`)
		if err := store.SaveFlow(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	return &ciFixture{t: t, ctx: ctx, store: store, svc: NewCIService(store, NewCloudDocumentProvider(store), NewAuthzService(store, collaboration.NewOrgService(store)), opts)}
}

func (f *ciFixture) mint(org string, days int) (*storageif.CICredential, string) {
	f.t.Helper()
	c, credential, err := f.svc.CreateCredential(f.ctx, org, "admin", "runner "+org, days)
	if err != nil {
		f.t.Fatalf("CreateCredential(%s): %v", org, err)
	}
	return c, credential
}

// signed is a call signed with key at now, naming flowID; keyID is sent as X-Baki-Key-Id when set.
func signed(key, keyID, flowID string) CIRequest {
	body := []byte(`{"flowId":"` + flowID + `","failOn":"error","format":"summary"}`)
	ts := time.Now().Unix()
	return CIRequest{KeyID: keyID, Timestamp: strconv.FormatInt(ts, 10), Signature: hmacsig.Sign([]byte(key), ts, body), Body: body}
}

func TestCIService_CreateCredential(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	c, credential := f.mint("org-a", 30)
	if !strings.HasPrefix(credential, CICredentialPrefix+c.ID+"_") || CIKeyID(credential) != c.ID || len(c.ID) != ciKeyIDLength {
		t.Errorf("credential %q does not carry its key id %q", credential, c.ID)
	}
	if c.Secret != "" || c.SecretHint != credential[len(credential)-4:] || c.Name != "runner org-a" || c.CreatedBy != "admin" {
		t.Errorf("returned credential = %+v", c)
	}
	if c.ExpiresAt == nil || c.ExpiresAt.Sub(c.CreatedAt) != 30*24*time.Hour {
		t.Errorf("ExpiresAt = %v for 30 days from %v", c.ExpiresAt, c.CreatedAt)
	}
	stored, err := f.store.GetCICredential(f.ctx, c.ID)
	if err != nil || stored.Secret != credential {
		t.Errorf("stored = %+v, %v; want the credential string as the secret", stored, err)
	}
	if never, _ := f.mint("org-a", 0); never.ExpiresAt != nil {
		t.Errorf("no expiry requested, ExpiresAt = %v", never.ExpiresAt)
	}
	_, other := f.mint("org-a", 0)
	if other == credential {
		t.Error("two credentials are equal")
	}

	for name, c := range map[string]struct {
		name string
		days int
	}{
		"empty name":      {"  ", 0},
		"long name":       {strings.Repeat("n", maxCICredentialName+1), 0},
		"negative expiry": {"x", -1},
		"too long expiry": {"x", MaxCICredentialDays + 1},
	} {
		if _, _, err := f.svc.CreateCredential(f.ctx, "org-a", "admin", c.name, c.days); !isKind(err, apperr.KindInvalid) {
			t.Errorf("%s: %v, want a 400", name, err)
		}
	}
	if _, _, err := f.svc.CreateCredential(f.ctx, "org-a", "admin", "max", MaxCICredentialDays); err != nil {
		t.Errorf("the longest lifetime: %v", err)
	}
}

func isKind(err error, kind apperr.Kind) bool {
	ae, ok := apperr.As(err)
	return ok && ae.Kind == kind
}

func TestCIService_CreateCredential_LimitAndEncryption(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	for range MaxActiveCICredentials {
		f.mint("org-a", 0)
	}
	_, _, err := f.svc.CreateCredential(f.ctx, "org-a", "admin", "one too many", 0)
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindConflict || ae.Code != "CI_CREDENTIAL_LIMIT" {
		t.Errorf("past the cap: %v, want 409 CI_CREDENTIAL_LIMIT", err)
	}
	f.mint("org-b", 0)

	f.store.FieldEncryptionOff = true
	_, _, err = f.svc.CreateCredential(f.ctx, "org-b", "admin", "plain", 0)
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindUnavailable || ae.Code != "CI_CREDENTIALS_UNAVAILABLE" {
		t.Errorf("without field encryption: %v, want 503 CI_CREDENTIALS_UNAVAILABLE", err)
	}
}

func TestCIService_RevokeCredential(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	c, credential := f.mint("org-a", 0)
	if err := f.svc.RevokeCredential(f.ctx, "org-b", c.ID); !isKind(err, apperr.KindNotFound) {
		t.Errorf("revoking from another organisation: %v, want 404", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, signed(credential, c.ID, "flow-a")); err != nil {
		t.Fatalf("before the revoke: %v", err)
	}
	if err := f.svc.RevokeCredential(f.ctx, "org-a", c.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, signed(credential, c.ID, "flow-a")); !errors.Is(err, ErrCIUnauthenticated) {
		t.Errorf("a revoked credential authenticated: %v", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, signed(credential, "", "flow-a")); !errors.Is(err, ErrCIUnauthenticated) {
		t.Errorf("a revoked credential authenticated through the flow: %v", err)
	}
	if err := f.svc.RevokeCredential(f.ctx, "org-a", c.ID); !isKind(err, apperr.KindNotFound) {
		t.Errorf("revoking twice: %v, want 404", err)
	}
}

func TestCIService_Authenticate(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true, GlobalSecret: secret.New("global-secret")})
	a, credA := f.mint("org-a", 0)
	b, credB := f.mint("org-b", 0)

	caller, err := f.svc.Authenticate(f.ctx, signed(credA, a.ID, "flow-a"))
	if err != nil || caller.Method != CIAuthKeyID || caller.OrgID != "org-a" || caller.CredentialID != a.ID || caller.Deprecated {
		t.Fatalf("key id = %+v, %v", caller, err)
	}
	if act := caller.Actor(); act.Kind != auth.ActorIntegration || act.OrgID != "org-a" || act.TokenID != a.ID || act.UserID != "" || act.Role != "" {
		t.Errorf("actor = %+v", act)
	}
	// An old client: no key id, found through the flow's organisation.
	if caller, err := f.svc.Authenticate(f.ctx, signed(credB, "", "flow-b")); err != nil || caller.Method != CIAuthFlowOrg || caller.OrgID != "org-b" || caller.CredentialID != b.ID {
		t.Errorf("flow fallback = %+v, %v", caller, err)
	}
	// The deployment secret still works, flagged deprecated where organisations have their own.
	if caller, err := f.svc.Authenticate(f.ctx, signed("global-secret", "", "flow-personal")); err != nil || caller.Method != CIAuthGlobal || caller.OrgID != "" || !caller.Deprecated {
		t.Errorf("global = %+v, %v", caller, err)
	}

	stale := signed(credA, a.ID, "flow-a")
	old := time.Now().Add(-hmacsig.MaxSkew - time.Minute).Unix()
	stale.Timestamp, stale.Signature = strconv.FormatInt(old, 10), hmacsig.Sign([]byte(credA), old, stale.Body)
	untimed := signed(credA, a.ID, "flow-a")
	untimed.Timestamp, untimed.Signature = "", hmacsig.SignUntimed([]byte(credA), untimed.Body)
	tampered := signed(credA, a.ID, "flow-a")
	tampered.Body = []byte(`{"flowId":"flow-b"}`)
	refused := map[string]CIRequest{
		"no signature":                     {KeyID: a.ID, Timestamp: signed(credA, "", "").Timestamp, Body: []byte(`{}`)},
		"wrong secret":                     signed(credA+"x", a.ID, "flow-a"),
		"another org's key id":             signed(credA, b.ID, "flow-a"),
		"unknown key id":                   signed(credA, "aaaaaaaaaaaaaaaa", "flow-a"),
		"malformed key id":                 signed(credA, "../../etc", "flow-a"),
		"stale timestamp":                  stale,
		"tampered body":                    tampered,
		"fallback: flow of another org":    signed(credA, "", "flow-b"),
		"fallback: personal flow":          signed(credA, "", "flow-personal"),
		"fallback: unknown flow":           signed(credA, "", "flow-missing"),
		"fallback: no flow id":             signed(credA, "", ""),
		"key id sent, no fallback past it": signed(credB, a.ID, "flow-b"),
		"bad signature, no key id":         {Timestamp: tampered.Timestamp, Signature: "sha256=00", Body: tampered.Body},
	}
	for name, req := range refused {
		if caller, err := f.svc.Authenticate(f.ctx, req); !errors.Is(err, ErrCIUnauthenticated) {
			t.Errorf("%s: %+v, %v; want ErrCIUnauthenticated", name, caller, err)
		}
	}
	// A credential never accepts the untimed form; it is told apart only as "no timestamp".
	if _, err := f.svc.Authenticate(f.ctx, untimed); !errors.Is(err, ErrCIMissingTimestamp) {
		t.Errorf("untimed: %v, want ErrCIMissingTimestamp", err)
	}

	// Expired: refused, by key id and through the flow alike.
	past := time.Now().Add(-time.Hour)
	expiredCred, expired := f.mint("org-a", 1)
	c, _ := f.store.GetCICredential(f.ctx, expiredCred.ID)
	c.ExpiresAt = &past
	if err := f.store.DeleteCICredential(f.ctx, "org-a", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateCICredential(f.ctx, c, MaxActiveCICredentials); err != nil {
		t.Fatal(err)
	}
	for _, keyID := range []string{c.ID, ""} {
		if _, err := f.svc.Authenticate(f.ctx, signed(expired, keyID, "flow-a")); !errors.Is(err, ErrCIUnauthenticated) {
			t.Errorf("expired credential (key id %q): %v", keyID, err)
		}
	}
}

func TestCIService_Authenticate_RecordsUseOffThePath(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	c, credential := f.mint("org-a", 0)
	if _, err := f.svc.Authenticate(f.ctx, signed(credential, c.ID, "flow-a")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.store.GetCICredential(f.ctx, c.ID)
		if got.LastUsedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the credential's use was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCIService_Authenticate_PerDeployment(t *testing.T) {
	// Without accounts there are no organisation credentials: the deployment secret is the only key, and is not
	// deprecated.
	f := newCIFixture(t, CIOptions{GlobalSecret: secret.New("global-secret")})
	if caller, err := f.svc.Authenticate(f.ctx, signed("global-secret", "", "flow-a")); err != nil || caller.Deprecated {
		t.Errorf("single tenant global = %+v, %v", caller, err)
	}
	withOrgs := newCIFixture(t, CIOptions{OrgCredentials: true})
	c, credential := withOrgs.mint("org-a", 0)
	noOrgs := &CIService{store: withOrgs.store, docs: withOrgs.svc.docs, opts: CIOptions{GlobalSecret: secret.New("global-secret")}, touches: withOrgs.svc.touches, now: time.Now}
	if _, err := noOrgs.Authenticate(withOrgs.ctx, signed(credential, c.ID, "flow-a")); !errors.Is(err, ErrCIUnauthenticated) {
		t.Errorf("an org credential on a deployment without accounts: %v", err)
	}

	// The untimed form only with the operator's opt-in, and only for the deployment secret.
	untimed := signed("global-secret", "", "flow-a")
	untimed.Timestamp, untimed.Signature = "", hmacsig.SignUntimed([]byte("global-secret"), untimed.Body)
	if _, err := f.svc.Authenticate(f.ctx, untimed); !errors.Is(err, ErrCIMissingTimestamp) {
		t.Errorf("untimed without the opt-in: %v", err)
	}
	legacy := newCIFixture(t, CIOptions{GlobalSecret: secret.New("global-secret"), AllowNoTimestamp: true})
	if caller, err := legacy.svc.Authenticate(legacy.ctx, untimed); err != nil || caller.Method != CIAuthGlobal {
		t.Errorf("untimed with the opt-in: %+v, %v", caller, err)
	}

	unconfigured := newCIFixture(t, CIOptions{})
	if unconfigured.svc.Configured() {
		t.Error("no secret and no accounts is configured")
	}
	if !withOrgs.svc.Configured() || !f.svc.Configured() {
		t.Error("accounts, or a secret, configure the webhook")
	}
	if NewCIService(nil, nil, nil, CIOptions{OrgCredentials: true, GlobalSecret: secret.New("x")}).Configured() {
		t.Error("without a store nothing is configured")
	}
}

func TestCIService_ResolveFlow(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true, GlobalSecret: secret.New("global-secret")})
	org := &CICaller{Method: CIAuthKeyID, OrgID: "org-a", CredentialID: "k"}
	doc, err := f.svc.ResolveFlow(f.ctx, org, "flow-a")
	if err != nil || doc.ID != "flow-a" || doc.OrganizationID != "org-a" || doc.Name != "A" {
		t.Fatalf("own organisation's flow = %+v, %v", doc, err)
	}
	for _, id := range []string{"flow-b", "flow-personal", "flow-missing"} {
		if doc, err := f.svc.ResolveFlow(f.ctx, org, id); !errors.Is(err, storageif.ErrNotFound) {
			t.Errorf("%s through org-a's credential = %+v, %v; want ErrNotFound", id, doc, err)
		}
	}
	global := &CICaller{Method: CIAuthGlobal}
	for _, id := range []string{"flow-a", "flow-b", "flow-personal"} {
		if doc, err := f.svc.ResolveFlow(f.ctx, global, id); err != nil || doc.ID != id {
			t.Errorf("%s through the deployment secret = %+v, %v", id, doc, err)
		}
	}
}

// Local mode's document provider holds the open document only; the webhook resolves the flow it names from the
// store instead (it used to analyse whatever was open).
func TestCIService_ResolveFlow_LocalModeReadsTheStore(t *testing.T) {
	f := newCIFixture(t, CIOptions{GlobalSecret: secret.New("global-secret")})
	local := NewLocalDocumentProvider()
	local.SetCurrentDoc(&models.FlowDocument{ID: "open", Name: "Open"})
	svc := NewCIService(f.store, local, nil, CIOptions{GlobalSecret: secret.New("global-secret")})
	doc, err := svc.ResolveFlow(f.ctx, &CICaller{Method: CIAuthGlobal}, "flow-b")
	if err != nil || doc.ID != "flow-b" || doc.Name != "B" {
		t.Errorf("ResolveFlow = %+v, %v; want the stored flow-b", doc, err)
	}
	if _, err := svc.ResolveFlow(f.ctx, &CICaller{Method: CIAuthGlobal}, "open"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("the open document is no stored flow: %v", err)
	}
}

func TestCIKeyID(t *testing.T) {
	for in, want := range map[string]string{
		"baki_ci_abcdefghijklmnop_c2VjcmV0":  "abcdefghijklmnop",
		"baki_ci_abcdefghijklmn23_x":         "abcdefghijklmn23",
		"baki_ci_abcdefghijklmnop":           "",
		"baki_ci_ABCDEFGHIJKLMNOP_secret":    "",
		"baki_ci_abcdefghijklmno1_secret":    "",
		"baki_ci_abcdefghijklmnopq_secret":   "",
		"topsecret":                          "",
		"xbaki_ci_abcdefghijklmnop_secret":   "",
		"baki_ci__abcdefghijklmnop_secret":   "",
		"baki_ci_abcdefghijklmnop_":          "abcdefghijklmnop",
		"baki_ci_abcdefghijklmnop_with_more": "abcdefghijklmnop",
	} {
		if got := CIKeyID(in); got != want {
			t.Errorf("CIKeyID(%q) = %q, want %q", in, got, want)
		}
	}
}
