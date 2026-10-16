package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
)

// loginRaceStore runs afterLoad once Authenticate has read the account: the window in which the login spends its
// bcrypt time before it writes the counter.
type loginRaceStore struct {
	storageif.StorageBackend
	afterLoad func(*storageif.User)
}

func (s *loginRaceStore) LoadUserByEmail(ctx context.Context, email string) (*storageif.User, error) {
	u, err := s.StorageBackend.LoadUserByEmail(ctx, email)
	if err == nil && s.afterLoad != nil {
		s.afterLoad(u)
	}
	return u, err
}

// forEachLoginBackend runs fn against the fake and, with DATABASE_URL, a migrated Postgres database of its own.
func forEachLoginBackend(t *testing.T, fn func(t *testing.T, b storageif.StorageBackend)) {
	t.Run("fake", func(t *testing.T) { fn(t, testutil.NewFakeBackend()) })
	t.Run("postgres", func(t *testing.T) {
		pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
		if err != nil {
			t.Fatalf("open the test database: %v", err)
		}
		t.Cleanup(func() { _ = pg.Close() })
		fn(t, pg)
	})
}

// seedLoginUser creates a member whose password is "right", hashed at the minimum cost.
func seedLoginUser(t *testing.T, b storageif.StorageBackend, id string, verified bool) *storageif.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u := &storageif.User{ID: id, Email: id + "@example.com", Password: string(hash), Role: auth.RoleMember, EmailVerified: verified}
	if err := b.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// TestAuthenticate_ParallelWrongPasswordsLockTheAccount: wrong passwords that all read the account before any of
// them writes must still add up. The counter used to be read, incremented in Go and written back with the whole
// row, so ten parallel attempts left it at one and the account never locked.
func TestAuthenticate_ParallelWrongPasswordsLockTheAccount(t *testing.T) {
	forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
		ctx := context.Background()
		u := seedLoginUser(t, b, "lockout-parallel", true)
		const n = 10
		var arrived sync.WaitGroup
		arrived.Add(n)
		store := &loginRaceStore{StorageBackend: b, afterLoad: func(*storageif.User) {
			arrived.Done()
			arrived.Wait() // every attempt has read the unlocked account before any writes
		}}
		svc := NewAuthService(store, nil)

		results := make([]LoginResult, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = svc.Authenticate(ctx, u.Email, "wrong")
			}()
		}
		wg.Wait()

		got, err := b.LoadUserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		if got.FailedLoginAttempts < failedLoginLockThreshold || got.LockedUntil == nil || !got.LockedUntil.After(time.Now()) {
			t.Fatalf("after %d parallel wrong passwords: %d attempts, locked until %v; want at least %d and a lock",
				n, got.FailedLoginAttempts, got.LockedUntil, failedLoginLockThreshold)
		}
		justLocked := 0
		for i, r := range results {
			if r.Outcome != LoginInvalidPassword && r.Outcome != LoginAccountLocked {
				t.Errorf("attempt %d: outcome %d, want invalid password or locked", i, r.Outcome)
			}
			if r.AccountJustLocked {
				justLocked++
			}
		}
		if justLocked != 1 {
			t.Errorf("%d attempts report locking the account, want exactly the one that crossed the threshold", justLocked)
		}

		store.afterLoad = nil
		if r := svc.Authenticate(ctx, u.Email, "right"); r.Outcome != LoginAccountLocked {
			t.Errorf("the right password during the lock: outcome %d, want LoginAccountLocked", r.Outcome)
		}
	})
}

// TestAuthenticate_LoginKeepsConcurrentAccountChanges: a password reset, role change, verification or profile edit
// that lands while a login is checking the password survives the login's counter write. The write used to put
// back the whole row the login had read, the old password hash included.
func TestAuthenticate_LoginKeepsConcurrentAccountChanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		priorBad int // wrong passwords before the raced login, so a successful one has a counter to reset
		want     LoginOutcome
		attempts int
	}{
		// The failed login races a password reset (which also clears the counter) and the owner's verification.
		{name: "failed login", password: "wrong", want: LoginInvalidPassword, attempts: 1},
		{name: "successful login", password: "right", priorBad: 2, want: LoginSuccess, attempts: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
				ctx := context.Background()
				raceReset := tc.priorBad == 0
				u := seedLoginUser(t, b, "lockout-keep", !raceReset)
				store := &loginRaceStore{StorageBackend: b}
				svc := NewAuthService(store, nil)
				for range tc.priorBad {
					if r := svc.Authenticate(ctx, u.Email, "wrong"); r.Outcome != LoginInvalidPassword {
						t.Fatalf("prior wrong password: outcome %d", r.Outcome)
					}
				}

				const newHash = "hash-set-by-the-reset"
				store.afterLoad = func(*storageif.User) {
					store.afterLoad = nil
					if raceReset {
						if err := b.UpdateUserPassword(ctx, u.ID, newHash); err != nil {
							t.Errorf("UpdateUserPassword: %v", err)
						}
						if err := b.SetUserEmailVerified(ctx, u.ID); err != nil {
							t.Errorf("SetUserEmailVerified: %v", err)
						}
					}
					if err := b.UpdateUserRole(ctx, u.ID, auth.RoleAdmin); err != nil {
						t.Errorf("UpdateUserRole: %v", err)
					}
					if err := b.UpdateUserProfile(ctx, u.ID, "Renamed", "https://example.com/a.png"); err != nil {
						t.Errorf("UpdateUserProfile: %v", err)
					}
				}
				if r := svc.Authenticate(ctx, u.Email, tc.password); r.Outcome != tc.want {
					t.Fatalf("raced login: outcome %d, want %d", r.Outcome, tc.want)
				}

				got, err := b.LoadUserByID(ctx, u.ID)
				if err != nil {
					t.Fatalf("LoadUserByID: %v", err)
				}
				if raceReset && (got.Password != newHash || !got.EmailVerified) {
					t.Errorf("the login's stale row overwrote the reset or the verification: new hash kept %v, verified %v",
						got.Password == newHash, got.EmailVerified)
				}
				if got.Role != auth.RoleAdmin || got.DisplayName != "Renamed" || got.AvatarURL != "https://example.com/a.png" {
					t.Errorf("concurrent changes lost: role %q, name %q, avatar %q", got.Role, got.DisplayName, got.AvatarURL)
				}
				if got.FailedLoginAttempts != tc.attempts || got.LockedUntil != nil {
					t.Errorf("counter: %d attempts, locked until %v; want %d and no lock", got.FailedLoginAttempts, got.LockedUntil, tc.attempts)
				}
			})
		})
	}
}

// avatarRenamingStore renames the user after every step of an avatar upload or removal, as a profile edit in another tab
// would, and remembers the last name written.
type avatarRenamingStore struct {
	*testutil.FakeBackend
	t    *testing.T
	n    int
	last string
}

func (s *avatarRenamingStore) rename(ctx context.Context, id string) {
	u, err := s.FakeBackend.LoadUserByID(ctx, id)
	if err != nil {
		s.t.Fatalf("rename: %v", err)
	}
	s.n++
	s.last = "Renamed " + string(rune('A'+s.n))
	if err := s.UpdateUserProfile(ctx, id, s.last, u.AvatarURL); err != nil {
		s.t.Fatalf("rename: %v", err)
	}
}

func (s *avatarRenamingStore) SaveUserAvatar(ctx context.Context, id, mimeType string, data []byte) error {
	err := s.FakeBackend.SaveUserAvatar(ctx, id, mimeType, data)
	s.rename(ctx, id)
	return err
}

func (s *avatarRenamingStore) DeleteUserAvatar(ctx context.Context, id string) error {
	err := s.FakeBackend.DeleteUserAvatar(ctx, id)
	s.rename(ctx, id)
	return err
}

func (s *avatarRenamingStore) LoadUserByID(ctx context.Context, id string) (*storageif.User, error) {
	u, err := s.FakeBackend.LoadUserByID(ctx, id)
	s.rename(ctx, id)
	return u, err
}

// TestAuthService_AvatarKeepsConcurrentRename: uploading or removing an avatar writes only the avatar URL, so a
// display-name change landing in between is kept. Both used to read the user and write the name they had read back.
func TestAuthService_AvatarKeepsConcurrentRename(t *testing.T) {
	ctx := context.Background()
	const (
		id  = "svc-avatar-race"
		url = "/api/users/svc-avatar-race/avatar"
	)
	store := &avatarRenamingStore{FakeBackend: testutil.NewFakeBackend(), t: t}
	store.Users = map[string]*storageif.User{id: {ID: id, Email: "svc-avatar-race@example.com", DisplayName: "Ada"}}
	svc := NewAuthService(store, nil)

	if err := svc.SaveAvatar(ctx, id, pngBytes(t, 16, 16), url); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}
	if u := store.Users[id]; u.DisplayName != store.last || u.AvatarURL != url {
		t.Errorf("after the upload: name %q, avatar %q; want the concurrent rename %q and the serving route", u.DisplayName, u.AvatarURL, store.last)
	}

	if err := svc.DeleteAvatar(ctx, id, url); err != nil {
		t.Fatalf("DeleteAvatar: %v", err)
	}
	if u := store.Users[id]; u.DisplayName != store.last || u.AvatarURL != "" {
		t.Errorf("after the removal: name %q, avatar %q; want the concurrent rename %q and no URL", u.DisplayName, u.AvatarURL, store.last)
	}
}

// TestAuthenticate_RightPasswordAfterAParallelLockIsRefused: the right password read the account before a burst of
// wrong ones locked it (they run while it spends its bcrypt time), so its own read says unlocked. It must not get a
// session once the lock is set: the success path checks the lock again against the row as it is now, in the write
// that clears the counter. It used to trust its read, so a burst of N parallel guesses had all N answered.
func TestAuthenticate_RightPasswordAfterAParallelLockIsRefused(t *testing.T) {
	forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
		ctx := context.Background()
		u := seedLoginUser(t, b, "lockout-burst", true)
		store := &loginRaceStore{StorageBackend: b}
		svc := NewAuthService(store, nil)
		burst := false
		store.afterLoad = func(*storageif.User) {
			if burst {
				return
			}
			burst = true // the first load is the right password's; the burst's own loads pass through
			for range failedLoginLockThreshold {
				svc.Authenticate(ctx, u.Email, "wrong")
			}
		}
		if r := svc.Authenticate(ctx, u.Email, "right"); r.Outcome != LoginAccountLocked {
			t.Errorf("the right password after the burst locked the account: outcome %d, want LoginAccountLocked (%d)", r.Outcome, LoginAccountLocked)
		}
		got, err := b.LoadUserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		if got.FailedLoginAttempts != failedLoginLockThreshold || got.LockedUntil == nil || !got.LockedUntil.After(time.Now()) {
			t.Errorf("after the refused login: %d attempts, locked until %v; want the burst's lock to stand", got.FailedLoginAttempts, got.LockedUntil)
		}
	})
}

// TestChangeRole_ConcurrentDemotionsKeepAnAdmin: the deployment's last two admins demote each other at the same time.
// Exactly one demotion is refused (ErrLastAdmin) and one admin remains. The check read the admin list and then
// wrote the role, so both read two admins and both demoted. On Postgres a transaction writing both admin rows holds
// the two demotions until both are in flight, so they overlap on every run: without the store's row locks both read
// two admins, and both demote once it commits.
func TestChangeRole_ConcurrentDemotionsKeepAnAdmin(t *testing.T) {
	ids := []string{"admin-a", "admin-b"}
	demoteBoth := func(t *testing.T, b storageif.StorageBackend, hold func() (inFlight func())) {
		ctx := context.Background()
		for _, id := range ids {
			if err := b.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "x", Role: auth.RoleAdmin}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
		}
		inFlight := hold()
		svc := NewUserAdminService(b)
		errs := make([]error, len(ids))
		var wg sync.WaitGroup
		for i, id := range ids {
			wg.Go(func() { errs[i] = svc.ChangeRole(ctx, id, auth.RoleMember) })
		}
		inFlight()
		wg.Wait()
		refused := 0
		for i, err := range errs {
			switch {
			case errors.Is(err, ErrLastAdmin):
				refused++
			case err != nil:
				t.Errorf("demote %s: %v", ids[i], err)
			}
		}
		admins, err := b.ListAdmins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if refused != 1 || len(admins) != 1 {
			t.Errorf("%d demotions refused, %d admins left; want exactly one of each", refused, len(admins))
		}
	}
	t.Run("fake", func(t *testing.T) {
		demoteBoth(t, testutil.NewFakeBackend(), func() func() { return func() {} })
	})
	t.Run("postgres", func(t *testing.T) {
		ctx := context.Background()
		dsn := pgtest.NewDatabase(t, "")
		pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
		if err != nil {
			t.Fatalf("open the test database: %v", err)
		}
		t.Cleanup(func() { _ = pg.Close() })
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		demoteBoth(t, pg, func() func() {
			holder, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, `UPDATE users SET updated_at = updated_at WHERE role = 'admin'`); err != nil {
				t.Fatal(err)
			}
			return func() {
				for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					// A transaction reads pg_stat_activity once, unless it drops that snapshot.
					if _, err := holder.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
						t.Fatal(err)
					}
					var waiting int
					if err := holder.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting >= 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%d demotions wait for the admin rows after 5 s, want both", waiting)
					}
				}
				if err := holder.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	})
}
