package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/storage/interfaces"
)

// Share tokens (desktop / single-user filesystem mode).
// Tokens live in one JSON file under share_tokens.json (a map keyed by token ID).

func (lsb *LocalStorageBackend) shareTokensPath() string {
	return filepath.Join(lsb.dataDir, "share_tokens.json")
}

func (lsb *LocalStorageBackend) readShareTokens() (map[string]*interfaces.ShareToken, error) {
	data, err := os.ReadFile(lsb.shareTokensPath())
	if os.IsNotExist(err) {
		return map[string]*interfaces.ShareToken{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]*interfaces.ShareToken
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (lsb *LocalStorageBackend) writeShareTokens(m map[string]*interfaces.ShareToken) error {
	path := lsb.shareTokensPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o644)
}

func (lsb *LocalStorageBackend) CreateShareToken(ctx context.Context, t *interfaces.ShareToken) error {
	lsb.shareTokenMu.Lock()
	defer lsb.shareTokenMu.Unlock()
	m, err := lsb.readShareTokens()
	if err != nil {
		return err
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	// Key by TokenHash (not ID) because TokenHash has json:"-" on the DTO
	// and would be lost during round-trip serialization.
	m[t.TokenHash] = t
	return lsb.writeShareTokens(m)
}

func (lsb *LocalStorageBackend) GetShareTokenByHash(ctx context.Context, tokenHash string) (*interfaces.ShareToken, error) {
	lsb.shareTokenMu.Lock()
	defer lsb.shareTokenMu.Unlock()
	m, err := lsb.readShareTokens()
	if err != nil {
		return nil, err
	}
	t, ok := m[tokenHash]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	// Restore the hash from the map key (json:"-" drops it during round-trip).
	t.TokenHash = tokenHash
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return nil, interfaces.ErrNotFound
	}
	return t, nil
}

func (lsb *LocalStorageBackend) ListShareTokens(ctx context.Context, flowID string) ([]*interfaces.ShareToken, error) {
	lsb.shareTokenMu.Lock()
	defer lsb.shareTokenMu.Unlock()
	m, err := lsb.readShareTokens()
	if err != nil {
		return nil, err
	}
	var out []*interfaces.ShareToken
	for _, t := range m {
		if t.FlowID == flowID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (lsb *LocalStorageBackend) RevokeShareToken(ctx context.Context, flowID, tokenID string) error {
	lsb.shareTokenMu.Lock()
	defer lsb.shareTokenMu.Unlock()
	m, err := lsb.readShareTokens()
	if err != nil {
		return err
	}
	for hash, t := range m {
		if t.ID == tokenID && t.FlowID == flowID {
			delete(m, hash)
		}
	}
	return lsb.writeShareTokens(m)
}
