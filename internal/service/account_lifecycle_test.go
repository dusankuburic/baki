package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

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

// Finalize runs in order after the delete: the lent credentials are disabled (each reported), then the hook runs
// with the caller's key scope.
func TestAccountLifecycle_DeleteAccount_PhasesInOrder(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	providers := newOrgProviderService(t, secrets, orgKeys)
	_ = orgKeys.SaveOrg("org-1", "azure-foundry", azureTestDoc, "u1")
	_ = orgKeys.SaveOrg("org-2", "azure-foundry", azureTestDoc, "u2")

	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}
	svc := NewAccountLifecycleService(backend, nil)
	svc.SetProviderService(providers)
	var events []string
	svc.OnUserDeleted(func(scope string) { events = append(events, "hook:"+scope) })

	err := svc.DeleteAccount(context.Background(), "u1", "scope-u1", func(ref storageif.OrgKeyRef) {
		events = append(events, "lent:"+ref.OrgID+"/"+ref.Provider)
	})
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(backend.DeletedUsers) != 1 || backend.DeletedUsers[0] != "u1" {
		t.Errorf("deleted = %v, want [u1]", backend.DeletedUsers)
	}
	want := []string{"lent:org-1/azure-foundry", "hook:scope-u1"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Errorf("events = %v, want %v", events, want)
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

	svc := NewAccountLifecycleService(failingDeleteStore{testutil.NewFakeBackend()}, nil)
	svc.SetProviderService(providers)
	hooked := false
	svc.OnUserDeleted(func(string) { hooked = true })
	err := svc.DeleteAccount(context.Background(), "u1", "u1", func(storageif.OrgKeyRef) { t.Error("lent credential reported after a failed delete") })
	if err == nil {
		t.Fatal("want the delete error")
	}
	if hooked {
		t.Error("hook ran after a failed delete")
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); !has {
		t.Error("lent credential disabled after a failed delete")
	}
}

// Without a provider service (bare tests, desktop) erasure still deletes and runs the hook.
func TestAccountLifecycle_DeleteAccount_NoProviders(t *testing.T) {
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}
	svc := NewAccountLifecycleService(backend, nil)
	var scopes []string
	svc.OnUserDeleted(func(scope string) { scopes = append(scopes, scope) })
	if err := svc.DeleteAccount(context.Background(), "u1", "u1", nil); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "u1" {
		t.Errorf("hook scopes = %v, want [u1]", scopes)
	}
}

func TestAccountLifecycle_Export(t *testing.T) {
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1", Email: "a@example.com"}}}
	svc := NewAccountLifecycleService(backend, nil)
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

	streaming := NewAccountLifecycleService(streamingStore{backend}, nil)
	if !streaming.StreamsExport() {
		t.Fatal("a streaming store must stream")
	}
	var buf bytes.Buffer
	if err := streaming.StreamExport(context.Background(), "u1", &buf); err != nil || buf.String() != `{"user":"u1"}` {
		t.Errorf("StreamExport = %q, %v", buf.String(), err)
	}
}
