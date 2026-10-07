package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// failingDeleteStore fails the erasure itself.
type failingDeleteStore struct{ *testutil.FakeBackend }

func (failingDeleteStore) DeleteUser(context.Context, string) error { return errors.New("db down") }

// streamingStore is a store that can stream the data-subject export.
type streamingStore struct{ *testutil.FakeBackend }

func (streamingStore) ExportUserDataTo(_ context.Context, userID string, w io.Writer) error {
	_, err := io.WriteString(w, `{"user":"`+userID+`"}`)
	return err
}

// Finalize runs in order after the delete: the lent credentials are disabled (each reported), then AccountErased
// is announced with the caller's key scope.
func TestAccountLifecycle_DeleteAccount_PhasesInOrder(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	providers := newOrgProviderService(t, secrets, orgKeys)
	_ = orgKeys.SaveOrg("org-1", "azure-foundry", azureTestDoc, "u1")
	_ = orgKeys.SaveOrg("org-2", "azure-foundry", azureTestDoc, "u2")

	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}
	bus := eventbus.New(nil, 0)
	svc := NewAccountLifecycleService(backend, nil, bus)
	svc.SetProviderService(providers)
	var seen []string
	eventbus.Subscribe(bus, eventbus.Sync, "test", func(_ context.Context, e events.AccountErased) {
		seen = append(seen, "erased:"+e.UserID+"/"+e.KeyScope)
	})

	err := svc.DeleteAccount(context.Background(), "u1", "scope-u1", func(ref storageif.OrgKeyRef) {
		seen = append(seen, "lent:"+ref.OrgID+"/"+ref.Provider)
	})
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(backend.DeletedUsers) != 1 || backend.DeletedUsers[0] != "u1" {
		t.Errorf("deleted = %v, want [u1]", backend.DeletedUsers)
	}
	want := []string{"lent:org-1/azure-foundry", "erased:u1/scope-u1"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("seen = %v, want %v", seen, want)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); has {
		t.Error("the erased user's lent credential is still live")
	}
	if has, _ := orgKeys.HasOrg("org-2", "azure-foundry"); !has {
		t.Error("another admin's credential was disabled")
	}
}

// A failed delete returns its error and finalizes nothing.
func TestAccountLifecycle_DeleteAccount_FailedDeleteSkipsFinalize(t *testing.T) {
	configureAzureForTests(t)
	orgKeys := testutil.NewFakeOrgSecretStore()
	providers := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	_ = orgKeys.SaveOrg("org-1", "azure-foundry", azureTestDoc, "u1")

	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	svc := NewAccountLifecycleService(failingDeleteStore{testutil.NewFakeBackend()}, nil, bus)
	svc.SetProviderService(providers)
	err := svc.DeleteAccount(context.Background(), "u1", "u1", func(storageif.OrgKeyRef) { t.Error("lent credential reported after a failed delete") })
	if err == nil {
		t.Fatal("want the delete error")
	}
	if got := rec.Events(); len(got) != 0 {
		t.Errorf("announced %v after a failed delete", got)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); !has {
		t.Error("lent credential disabled after a failed delete")
	}
}

// Without a provider service (bare tests, desktop) erasure still deletes and is announced.
func TestAccountLifecycle_DeleteAccount_NoProviders(t *testing.T) {
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	svc := NewAccountLifecycleService(backend, nil, bus)
	if err := svc.DeleteAccount(context.Background(), "u1", "u1", nil); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if got := eventbus.Recorded[events.AccountErased](rec); len(got) != 1 || got[0] != (events.AccountErased{UserID: "u1", KeyScope: "u1"}) {
		t.Errorf("announced %v, want one AccountErased{u1, u1}", got)
	}
}

// The provider service drops an erased account's cached listings on AccountErased.
func TestAccountErased_DropsTheScopeListings(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	providers := newProviderServiceWithFakeSecrets(t, secrets)
	bus := eventbus.New(nil, 0)
	SubscribeModelCache(bus, providers)
	providers.modelCache["u1\x00openai"] = modelCacheEntry{expiresAt: time.Now().Add(time.Hour)}
	providers.modelCache["u2\x00openai"] = modelCacheEntry{expiresAt: time.Now().Add(time.Hour)}
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}
	if err := NewAccountLifecycleService(backend, nil, bus).DeleteAccount(context.Background(), "u1", "u1", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := providers.modelCache["u1\x00openai"]; ok {
		t.Error("the erased account's listing is still cached")
	}
	if _, ok := providers.modelCache["u2\x00openai"]; !ok {
		t.Error("another account's listing was dropped")
	}
}

func TestAccountLifecycle_Export(t *testing.T) {
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1", Email: "a@example.com"}}}
	svc := NewAccountLifecycleService(backend, nil, nil)
	if svc.StreamsExport() {
		t.Error("the fake cannot stream")
	}
	export, err := svc.Export(context.Background(), "u1")
	if err != nil || export.User == nil || export.User.ID != "u1" {
		t.Fatalf("Export = %+v, %v", export, err)
	}
	if _, err := svc.Export(context.Background(), "nobody"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("unknown user: %v, want ErrNotFound", err)
	}

	streaming := NewAccountLifecycleService(streamingStore{backend}, nil, nil)
	if !streaming.StreamsExport() {
		t.Fatal("a streaming store must stream")
	}
	var buf bytes.Buffer
	if err := streaming.StreamExport(context.Background(), "u1", &buf); err != nil || buf.String() != `{"user":"u1"}` {
		t.Errorf("StreamExport = %q, %v", buf.String(), err)
	}
}
