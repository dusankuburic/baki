package contract

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runAPITokenScenarios holds the API token lifecycle.
func runAPITokenScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("APIToken_create_lookup_list_delete", func(t *testing.T) {
		// A token belongs to a user; create the owner so the FK (Postgres) holds.
		owner := "contract-token-owner-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: owner, Email: "contract-token-" + runID + "@example.com", Password: "hash",
		}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		hash := "contract-token-hash-" + runID
		tok := &interfaces.APIToken{ID: "contract-tok-" + runID, UserID: owner, Name: "ci", TokenHash: hash}
		if err := b.CreateAPIToken(ctx, tok); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}

		got, err := b.GetAPITokenByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetAPITokenByHash: %v", err)
		}
		if got.UserID != owner || got.Name != "ci" {
			t.Errorf("token round-trip mismatch: %+v", got)
		}

		if _, err := b.GetAPITokenByHash(ctx, "no-such-hash-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("missing token hash: expected ErrNotFound, got %v", err)
		}

		list, err := b.ListAPITokens(ctx, owner)
		if err != nil {
			t.Fatalf("ListAPITokens: %v", err)
		}
		if len(list) != 1 || list[0].ID != tok.ID {
			t.Fatalf("expected the owner's single token, got %+v", list)
		}

		if err := b.DeleteAPIToken(ctx, "someone-else-"+runID, tok.ID); err != nil {
			t.Fatalf("DeleteAPIToken (wrong owner): %v", err)
		}
		if _, err := b.GetAPITokenByHash(ctx, hash); err != nil {
			t.Error("token must survive a delete attempt by a non-owner")
		}

		if err := b.DeleteAPIToken(ctx, owner, tok.ID); err != nil {
			t.Fatalf("DeleteAPIToken: %v", err)
		}
		if _, err := b.GetAPITokenByHash(ctx, hash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("deleted token: expected ErrNotFound, got %v", err)
		}
	})
}
