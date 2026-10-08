// Package filesystem reads the data directory a legacy filesystem-backed server left behind (PAD_DATA_DIR), for the
// legacy migrator. It only reads: the backend that wrote the directory is gone.
package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/atomicfile"
	"pad-analyzer/internal/storage/interfaces"
)

// LegacySource reads the data directory of a legacy filesystem-backed server for the migrator, and only reads it:
// it never creates, writes, renames or removes anything there, keeps no index and caches nothing, so a directory
// mounted read-only, or one an old build is still serving, migrates as it is.
//
// The layout is the one old builds wrote: flows/<id>.json, settings.json, and conversations/<scope>/<flowID>.json.
type LegacySource struct {
	dataDir string
}

var _ migration.LegacySource = (*LegacySource)(nil)

// OpenLegacySource opens dataDir for reading. It does not create a missing directory (the old backend did, which
// turned a mistyped PAD_DATA_DIR into an empty migration that reported success).
func OpenLegacySource(dataDir string) (*LegacySource, error) {
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("legacy data directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("legacy data directory %s is not a directory", dataDir)
	}
	return &LegacySource{dataDir: dataDir}, nil
}

// ListFlowIDs names flows by their file names and reads no file, so a flow that cannot be decoded is still listed
// (LoadFlow then reports it). The names are sorted as IDs: the directory's own order compares the ".json" suffix
// too, which puts "a-b" before "a".
func (s *LegacySource) ListFlowIDs(ctx context.Context, afterID string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "flows"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("read flows directory: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok || id <= afterID {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// LoadFlow reads flows/<id>.json. The file name is the flow's identity in this layout (the writer named each file
// after its flow and listed flows by file name), so the flow is returned under id even if the body says otherwise:
// a rerun looks the copy up by that name.
func (s *LegacySource) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !safePathSegment(id) {
		return nil, fmt.Errorf("invalid flow id %q", id)
	}
	data, err := atomicfile.ReadFileTolerant(filepath.Join(s.dataDir, "flows", id+".json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("read flow file: %w", err)
	}
	var flow interfaces.FlowDocument
	if err := json.Unmarshal(data, &flow); err != nil {
		return nil, fmt.Errorf("decode flow file: %w", err)
	}
	flow.ID = id
	return &flow, nil
}

// LoadSettings reads settings.json. A directory without one holds no settings: unlike the old backend this does
// not answer with defaults, which the migrator would copy over the destination's own settings.
func (s *LegacySource) LoadSettings(ctx context.Context) (*interfaces.AppSettings, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := atomicfile.ReadFileTolerant(filepath.Join(s.dataDir, "settings.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("read settings file: %w", err)
	}
	var settings interfaces.AppSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("decode settings file: %w", err)
	}
	return &settings, nil
}

// ListConversations lists conversations/<scope>/<flowID>.json, sorted by scope and then file name. Files at any
// other depth were never written by a build and are not listed.
func (s *LegacySource) ListConversations(ctx context.Context) ([]migration.ConversationRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(s.dataDir, "conversations")
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []migration.ConversationRef{}, nil
		}
		return nil, fmt.Errorf("stat conversations dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("conversations path is not a directory: %s", root)
	}
	scopes, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read conversations dir: %w", err)
	}
	refs := []migration.ConversationRef{}
	for _, scope := range scopes {
		if !scope.IsDir() {
			continue
		}
		dir := filepath.Join(root, scope.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read conversations dir: %w", err)
		}
		for _, f := range files {
			flowID, ok := strings.CutSuffix(f.Name(), ".json")
			if f.IsDir() || !ok {
				continue
			}
			refs = append(refs, migration.ConversationRef{Scope: scope.Name(), FlowID: flowID, Path: filepath.Join(dir, f.Name())})
		}
	}
	return refs, nil
}

// LoadConversation reads one conversation in either shape an old build wrote: the chat service's file
// (models.ConversationFile: version, flowKey, scope and the messages) or the bare message list the old backend's
// SaveConversation wrote. The file's flowKey and scope, when set, win over the path.
func (s *LegacySource) LoadConversation(ctx context.Context, ref migration.ConversationRef) (*migration.LegacyConversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !safePathSegment(ref.Scope) || !safePathSegment(ref.FlowID) {
		return nil, fmt.Errorf("invalid conversation identifier")
	}
	data, err := atomicfile.ReadFileTolerant(filepath.Join(s.dataDir, "conversations", ref.Scope, ref.FlowID+".json"))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	conv := &migration.LegacyConversation{FlowID: ref.FlowID, Scope: ref.Scope}
	if body := bytes.TrimSpace(data); len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &conv.Messages); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
	} else {
		var file struct {
			FlowKey  string               `json:"flowKey"`
			Scope    string               `json:"scope"`
			Messages []legacyModelMessage `json:"messages"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		if file.FlowKey != "" {
			conv.FlowID = file.FlowKey
		}
		if file.Scope != "" {
			conv.Scope = file.Scope
		}
		conv.Messages = make([]interfaces.ChatMessage, len(file.Messages))
		for i, m := range file.Messages {
			conv.Messages[i] = m.ChatMessage
			if !m.Timestamp.IsZero() {
				conv.Messages[i].Timestamp = m.Timestamp.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	if conv.Messages == nil {
		conv.Messages = []interfaces.ChatMessage{}
	}
	return conv, nil
}

// legacyModelMessage decodes a models.ChatMessage straight into the storage shape: every JSON key is shared, and
// only the timestamp differs (a time there, a string here), so the outer field takes it. The chat service converts
// the same way when it saves (UTC, RFC 3339 with nanoseconds, empty for the zero time).
type legacyModelMessage struct {
	interfaces.ChatMessage
	Timestamp time.Time `json:"timestamp"`
}

// safePathSegment reports whether s is safe as a single on-disk path segment (no separators, no parent-dir escape,
// non-empty): flow IDs and conversation scopes come from the listing, but LoadFlow and LoadConversation also take
// them from callers.
func safePathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}
