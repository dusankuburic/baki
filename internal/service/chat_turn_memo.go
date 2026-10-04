package service

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/singleflight"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// turnMemo holds the per-turn reads a chat turn used to repeat before the provider was
// dialled: the caller's settings row (LoadUserSettings, read once for the
// start-of-stream persist and again for the custom instructions) and the caller's org
// role in the flow's organisation (the registry's, the factory's and the knowledge-base
// gate's lookups each listed every org of the caller). It rides the turn's context
// (withTurnMemo) for one turn only; everything outside a turn reads fresh. Safe for
// concurrent use: the RAG lookup runs beside the worker.
type turnMemo struct {
	prefsOnce sync.Once
	prefs     *models.AppSettings

	mu     sync.Mutex
	roles  map[string]memoRole // orgID + "\x00" + userID → a successful lookup
	flight singleflight.Group
}

// memoRole is one successful org-role lookup (ok=false: not a member).
type memoRole struct {
	role auth.Role
	ok   bool
}

type turnMemoKey struct{}

// withTurnMemo returns ctx carrying a fresh turnMemo.
func withTurnMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnMemoKey{}, &turnMemo{roles: make(map[string]memoRole)})
}

// withoutTurnMemo returns ctx with the turn's memo masked: a read after the turn (the
// answer persisted at stream end) sees the settings as they are then — a "Save
// conversation history" switched off mid-answer still keeps the answer out of the store.
func withoutTurnMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnMemoKey{}, (*turnMemo)(nil))
}

func turnMemoFrom(ctx context.Context) *turnMemo {
	m, _ := ctx.Value(turnMemoKey{}).(*turnMemo)
	return m
}

// prefetchCallerPreferences starts the caller's settings read in the background so it
// overlaps the turn's provider setup and budget check (prepareTurn); the turn's first
// reader then waits for it instead of issuing it. No memo in ctx: nothing to do.
// Only cloud reads a store (desktop's settings are in memory).
func (s *ChatService) prefetchCallerPreferences(ctx context.Context) {
	if s.mode != config.ModeCloud || turnMemoFrom(ctx) == nil {
		return
	}
	go s.callerPreferences(ctx)
}

// orgRoleCached is lookupOrgRoleUncached through the turn's memo: one store read per
// (org, user) per turn. Failed reads are not memoised — the next reader tries again,
// as it always did.
func (s *ChatService) orgRoleCached(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	memo := turnMemoFrom(ctx)
	if memo == nil {
		return s.lookupOrgRoleUncached(ctx, orgID, userID)
	}
	key := orgID + "\x00" + userID
	memo.mu.Lock()
	r, hit := memo.roles[key]
	memo.mu.Unlock()
	if hit {
		return r.role, r.ok, nil
	}
	v, err, _ := memo.flight.Do(key, func() (any, error) {
		role, ok, err := s.lookupOrgRoleUncached(ctx, orgID, userID)
		if err != nil {
			return nil, err
		}
		r := memoRole{role: role, ok: ok}
		memo.mu.Lock()
		memo.roles[key] = r
		memo.mu.Unlock()
		return r, nil
	})
	if err != nil {
		return "", false, err
	}
	r = v.(memoRole)
	return r.role, r.ok, nil
}

// lookupOrgRoleUncached reads userID's role in orgID from the org store. A full
// organisation store (both real backends; see main.go's org service) answers through
// collaboration.OrgService.MemberRole — one org's row and members instead of every org
// the caller belongs to with all their members; a narrower backend (test doubles) keeps
// the ListOrgsForUser scan. ok is false outside the org (unknown org included), in
// local mode and on a backend without either.
func (s *ChatService) lookupOrgRoleUncached(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	if s.backend == nil || orgID == "" || userID == "" {
		return "", false, nil
	}
	if store, isStore := s.backend.(collaboration.OrgStore); isStore {
		role, err := collaboration.NewOrgService(store).MemberRole(ctx, orgID, userID)
		switch {
		case err == nil:
			return role, true, nil
		case errors.Is(err, collaboration.ErrMemberNotFound), errors.Is(err, collaboration.ErrOrgNotFound), errors.Is(err, interfaces.ErrNotFound):
			return "", false, nil
		default:
			return "", false, err
		}
	}
	orgs, isLister := s.backend.(interface {
		ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error)
	})
	if !isLister {
		return "", false, nil
	}
	list, err := orgs.ListOrgsForUser(ctx, userID)
	if err != nil {
		return "", false, err
	}
	for _, o := range list {
		if o == nil || o.ID != orgID {
			continue
		}
		for _, m := range o.Members {
			if m.UserID == userID {
				return m.Role, true, nil
			}
		}
		// Listed among the caller's organisations without a member row: a member whose role
		// the listing does not carry — no role, so never the shared credential (OrgRoleUsesSharedCredential).
		return "", true, nil
	}
	return "", false, nil
}
