package filesystem

import (
	"context"
	"fmt"
	"sort"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// API tokens (filesystem/test backend). Held in memory, mirroring how users and
// orgs are kept here: the token hash carries a json:"-" tag (so it's never leaked
// in API responses) and therefore can't be round-tripped through JSON files. The
// filesystem backend is a single-user/desktop and test-only target — desktop mode
// has no auth, so machine tokens are exercised only by tests, where in-memory is
// sufficient.

func (lsb *LocalStorageBackend) CreateAPIToken(ctx context.Context, t *interfaces.APIToken) error {
	if t == nil || t.ID == "" || t.TokenHash == "" {
		return fmt.Errorf("api token requires id and tokenHash")
	}
	lsb.apiTokenMu.Lock()
	defer lsb.apiTokenMu.Unlock()

	if lsb.apiTokens == nil {
		lsb.apiTokens = make(map[string]*interfaces.APIToken)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	cp := *t
	lsb.apiTokens[t.ID] = &cp
	return nil
}

func (lsb *LocalStorageBackend) GetAPITokenByHash(ctx context.Context, tokenHash string) (*interfaces.APIToken, error) {
	lsb.apiTokenMu.Lock()
	defer lsb.apiTokenMu.Unlock()

	for _, t := range lsb.apiTokens {
		if t.TokenHash == tokenHash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) ListAPITokens(ctx context.Context, userID string) ([]*interfaces.APIToken, error) {
	lsb.apiTokenMu.Lock()
	defer lsb.apiTokenMu.Unlock()

	out := make([]*interfaces.APIToken, 0)
	for _, t := range lsb.apiTokens {
		if t.UserID == userID {
			cp := *t
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (lsb *LocalStorageBackend) DeleteAPIToken(ctx context.Context, userID, id string) error {
	lsb.apiTokenMu.Lock()
	defer lsb.apiTokenMu.Unlock()

	if t, ok := lsb.apiTokens[id]; ok && t.UserID == userID {
		delete(lsb.apiTokens, id)
	}
	return nil
}
