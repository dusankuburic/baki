package service

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"

	"pad-core/models"
)

// targetingNotifier records the TARGET of every delivery, which is the whole
// point of these tests — testutil.CountingNotifier collapses EmitTo into Emit
// and so cannot tell a broadcast from a scoped send.
type targetingNotifier struct {
	mu        sync.Mutex
	broadcast []string          // event names sent to every client
	targeted  map[string]string // userID → last event name
}

func newTargetingNotifier() *targetingNotifier {
	return &targetingNotifier{targeted: map[string]string{}}
}

func (n *targetingNotifier) Emit(name string, _ any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.broadcast = append(n.broadcast, name)
}

func (n *targetingNotifier) EmitTo(userID, name string, _ any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.targeted[userID] = name
}

func (n *targetingNotifier) HasSubscriber(string) bool { return true }

func (n *targetingNotifier) targets() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.targeted))
	for id := range n.targeted {
		out = append(out, id)
	}
	return out
}

// orgBackend serves one organisation so the notifier fan-out has members to
// address. Everything else falls through to FakeBackend.
type orgBackend struct {
	testutil.FakeBackend
	org      *storageif.Organisation
	loadErr  error
	loadCall int
}

func (b *orgBackend) LoadOrg(_ context.Context, _ string) (*storageif.Organisation, error) {
	b.loadCall++
	if b.loadErr != nil {
		return nil, b.loadErr
	}
	return b.org, nil
}

func newOrgSystemService(t *testing.T, backend storageif.StorageBackend, n EventNotifier) *SystemService {
	t.Helper()
	return NewSystemService(newTestSettingsStore(t), nil, n, backend, config.ModeCloud, nil)
}

// TestUpdateOrgSettings_NotifiesOnlyOrgMembers is the tenant-boundary gate.
// The event used to go out as a bare Emit, which reaches EVERY connected SSE
// client in the deployment — so one org's rule profile (which rules it
// disabled, at what severity) was delivered to every other tenant's users.
func TestUpdateOrgSettings_NotifiesOnlyOrgMembers(t *testing.T) {
	backend := &orgBackend{org: &storageif.Organisation{
		ID:      "org-a",
		OwnerID: "owner-1",
		Members: []storageif.OrgMember{{UserID: "member-1"}, {UserID: "member-2"}},
	}}
	n := newTargetingNotifier()
	svc := newOrgSystemService(t, backend, n)

	if err := svc.UpdateOrgSettings(context.Background(), "org-a", *models.DefaultSettings()); err != nil {
		t.Fatalf("UpdateOrgSettings: %v", err)
	}

	if len(n.broadcast) != 0 {
		t.Errorf("org settings were broadcast to every client: %v", n.broadcast)
	}
	want := map[string]bool{"owner-1": true, "member-1": true, "member-2": true}
	got := n.targets()
	if len(got) != len(want) {
		t.Fatalf("addressed %v, want exactly %d org members", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("addressed %q, who is not in the org", id)
		}
	}
}

// TestUpdateOrgSettings_OwnerNotDuplicated covers the owner who is also listed
// as a member: a double EmitTo would deliver the same event twice to one client.
func TestUpdateOrgSettings_OwnerNotDuplicated(t *testing.T) {
	backend := &orgBackend{org: &storageif.Organisation{
		ID:      "org-a",
		OwnerID: "owner-1",
		Members: []storageif.OrgMember{{UserID: "owner-1"}, {UserID: "member-1"}},
	}}
	n := newTargetingNotifier()
	svc := newOrgSystemService(t, backend, n)

	if err := svc.UpdateOrgSettings(context.Background(), "org-a", *models.DefaultSettings()); err != nil {
		t.Fatalf("UpdateOrgSettings: %v", err)
	}
	if got := len(n.targets()); got != 2 {
		t.Errorf("addressed %d distinct users, want 2 (owner listed twice must dedupe): %v", got, n.targets())
	}
}

// TestUpdateOrgSettings_NotifyFailureDoesNotFailTheWrite pins the best-effort
// contract: the settings are already persisted when the fan-out runs, so a
// storage hiccup resolving members must not turn a successful save into an
// error the admin sees.
func TestUpdateOrgSettings_NotifyFailureDoesNotFailTheWrite(t *testing.T) {
	backend := &orgBackend{loadErr: storageif.ErrNotFound}
	n := newTargetingNotifier()
	svc := newOrgSystemService(t, backend, n)

	if err := svc.UpdateOrgSettings(context.Background(), "org-a", *models.DefaultSettings()); err != nil {
		t.Fatalf("UpdateOrgSettings must succeed when only the notification fails: %v", err)
	}
	if backend.loadCall == 0 {
		t.Error("expected the fan-out to attempt a member lookup")
	}
	if len(n.broadcast) != 0 {
		t.Errorf("must not fall back to a broadcast when members are unknown: %v", n.broadcast)
	}
	if len(n.targets()) != 0 {
		t.Errorf("no members resolved, so nothing should have been addressed: %v", n.targets())
	}
}

// TestUpdateUserSettings_StaysUserScoped guards the neighbouring path that was
// already correct, so a future refactor of this file cannot regress it.
func TestUpdateUserSettings_StaysUserScoped(t *testing.T) {
	n := newTargetingNotifier()
	svc := newOrgSystemService(t, &testutil.FakeBackend{}, n)

	if err := svc.UpdateUserSettings(context.Background(), "user-9", *models.DefaultSettings()); err != nil {
		t.Fatalf("UpdateUserSettings: %v", err)
	}
	if len(n.broadcast) != 0 {
		t.Errorf("user settings were broadcast: %v", n.broadcast)
	}
	if got := n.targets(); len(got) != 1 || got[0] != "user-9" {
		t.Errorf("addressed %v, want only user-9", got)
	}
}
