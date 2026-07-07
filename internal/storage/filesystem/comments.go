package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/storage/interfaces"
)

// Finding comments (desktop / single-user filesystem mode).
// Comments live in one JSON file per flow under comments/<flowID>.json.

func (lsb *LocalStorageBackend) commentsPath(flowID string) (string, error) {
	if !safePathSegment(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(lsb.dataDir, "comments", flowID+".json"), nil
}

func (lsb *LocalStorageBackend) readComments(flowID string) (map[string]*interfaces.FindingComment, error) {
	path, err := lsb.commentsPath(flowID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path derived from dataDir + validated flowID
	if os.IsNotExist(err) {
		return map[string]*interfaces.FindingComment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]*interfaces.FindingComment
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (lsb *LocalStorageBackend) writeComments(flowID string, m map[string]*interfaces.FindingComment) error {
	path, err := lsb.commentsPath(flowID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

func (lsb *LocalStorageBackend) AddFindingComment(ctx context.Context, c *interfaces.FindingComment) error {
	lsb.commentsMu.Lock()
	defer lsb.commentsMu.Unlock()
	m, err := lsb.readComments(c.FlowID)
	if err != nil {
		return err
	}
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	m[c.ID] = c
	return lsb.writeComments(c.FlowID, m)
}

func (lsb *LocalStorageBackend) ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*interfaces.FindingComment, error) {
	lsb.commentsMu.Lock()
	defer lsb.commentsMu.Unlock()
	m, err := lsb.readComments(flowID)
	if err != nil {
		return nil, err
	}
	var out []*interfaces.FindingComment
	for _, c := range m {
		if c.FindingKey == findingKey {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (lsb *LocalStorageBackend) DeleteFindingComment(ctx context.Context, flowID, commentID, authorID string) error {
	lsb.commentsMu.Lock()
	defer lsb.commentsMu.Unlock()
	m, err := lsb.readComments(flowID)
	if err != nil {
		return err
	}
	c, ok := m[commentID]
	if !ok {
		return nil
	}
	if authorID != "" && c.AuthorID != authorID {
		return interfaces.ErrNotCommentAuthor
	}
	delete(m, commentID)
	return lsb.writeComments(flowID, m)
}
