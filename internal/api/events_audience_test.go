package api

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// audienceManager is an EventManager with one buffered client per entry of users (a user may repeat: several
// tabs), its roster from rosters.
func audienceManager(t *testing.T, rosters map[string][]string, users ...string) (*EventManager, []chan []byte, *int) {
	t.Helper()
	reads := 0
	m := NewEventManager(make(chan struct{}), WithOrgRoster(func(_ context.Context, orgID string) ([]string, error) {
		reads++
		ids, ok := rosters[orgID]
		if !ok {
			return nil, storageif.ErrNotFound
		}
		return ids, nil
	}))
	chans := make([]chan []byte, len(users))
	m.clientsMu.Lock()
	for i, u := range users {
		chans[i] = make(chan []byte, 8)
		m.addClientLocked(chans[i], u)
	}
	m.clientsMu.Unlock()
	return m, chans, &reads
}

func frames(chans []chan []byte) []int {
	out := make([]int, len(chans))
	for i, ch := range chans {
		out[i] = len(ch)
	}
	return out
}

// An empty user id is the local user (clients that connected without an identity), never a broadcast: where every
// client has a user, an event for it reaches no one.
func TestEmitTo_EmptyUserIsTheLocalUserNotABroadcast(t *testing.T) {
	m, chans, _ := audienceManager(t, nil, "alice", "bob")
	m.EmitTo(service.LocalUser, "flow:loaded", "x")
	if got := frames(chans); !slices.Equal(got, []int{0, 0}) {
		t.Fatalf("an event for the local user reached identified clients: %v", got)
	}

	local, localChans, _ := audienceManager(t, nil, "", "")
	local.EmitTo(service.LocalUser, "flow:loaded", "x")
	if got := frames(localChans); !slices.Equal(got, []int{1, 1}) {
		t.Fatalf("desktop clients got %v frames, want one each", got)
	}
}

// EmitToOrg reaches each member of the organisation (the owner too) once per connection, and no one outside it,
// however the roster lists them.
func TestEmitToOrg_DeliversToTheRosterOnly(t *testing.T) {
	rosters := map[string][]string{
		"org-a": {"owner-1", "member-1", "owner-1", ""},
		"org-b": {"outsider"},
	}
	m, chans, reads := audienceManager(t, rosters, "owner-1", "member-1", "member-1", "outsider", "")
	m.EmitToOrg(context.Background(), "org-a", "settings:changed", map[string]string{"scope": "org"})
	if got, want := frames(chans), []int{1, 1, 1, 0, 0}; !slices.Equal(got, want) {
		t.Fatalf("frames per client %v, want %v (owner, member's two tabs; not the other org, not the local user)", got, want)
	}
	if *reads != 1 {
		t.Fatalf("roster read %d times, want once per event", *reads)
	}
}

// A roster that cannot be read, or a deployment without one: nothing is delivered, and nothing falls back to a
// broadcast.
func TestEmitToOrg_NoRosterNoDelivery(t *testing.T) {
	m, chans, reads := audienceManager(t, map[string][]string{}, "owner-1", "member-1")
	m.EmitToOrg(context.Background(), "org-gone", "settings:changed", nil)
	if got := frames(chans); !slices.Equal(got, []int{0, 0}) {
		t.Fatalf("an unreadable roster delivered %v", got)
	}
	if *reads != 1 {
		t.Fatalf("roster read %d times, want 1", *reads)
	}

	bare := NewEventManager(make(chan struct{}))
	ch := make(chan []byte, 1)
	bare.clientsMu.Lock()
	bare.addClientLocked(ch, "owner-1")
	bare.clientsMu.Unlock()
	bare.EmitToOrg(context.Background(), "org-a", "settings:changed", nil)
	if len(ch) != 0 {
		t.Fatal("an EventManager without a roster delivered an org event")
	}
}

type orgLoaderStub struct {
	org *storageif.Organisation
	err error
}

func (s orgLoaderStub) LoadOrg(context.Context, string) (*storageif.Organisation, error) {
	return s.org, s.err
}

// The store's roster is the owner, who need not be listed as a member, plus the members.
func TestOrgRosterOf_OwnerAndMembers(t *testing.T) {
	roster := OrgRosterOf(orgLoaderStub{org: &storageif.Organisation{ID: "org-a", OwnerID: "owner-1", Members: []storageif.OrgMember{{UserID: "member-1"}, {UserID: "member-2"}}}})
	ids, err := roster(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if want := []string{"member-1", "member-2", "owner-1"}; !slices.Equal(ids, want) {
		t.Fatalf("roster %v, want %v", ids, want)
	}

	boom := errors.New("boom")
	if _, err := OrgRosterOf(orgLoaderStub{err: boom})(context.Background(), "org-a"); !errors.Is(err, boom) {
		t.Fatalf("load error = %v, want boom", err)
	}
	if _, err := OrgRosterOf(orgLoaderStub{})(context.Background(), "org-a"); !errors.Is(err, storageif.ErrNotFound) {
		t.Fatalf("missing organisation = %v, want ErrNotFound", err)
	}
}
