package contract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runCommentScenarios holds the finding comments.
func runCommentScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("FindingComments_delete_is_author_scoped_and_moderated", func(t *testing.T) {
		author, other := "contract-comment-author-"+runID, "contract-comment-other-"+runID
		flowID := "contract-flow-comments-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Comment Flow", Content: json.RawMessage(`{}`), OwnerID: author,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(ctx, flowID) })
		add := func(by string) string {
			c := &interfaces.FindingComment{FlowID: flowID, FindingKey: "k", AuthorID: by, Body: "by " + by}
			if err := b.AddFindingComment(ctx, c); err != nil {
				t.Fatalf("AddFindingComment: %v", err)
			}
			if c.ID == "" {
				t.Fatal("AddFindingComment left the ID empty")
			}
			return c.ID
		}
		count := func() int {
			cs, err := b.ListFindingComments(ctx, flowID, "k")
			if err != nil {
				t.Fatalf("ListFindingComments: %v", err)
			}
			return len(cs)
		}
		own, others := add(author), add(other)

		// An empty author is no one: it can never delete someone's comment.
		if err := b.DeleteFindingComment(ctx, flowID, others, ""); !errors.Is(err, interfaces.ErrNotCommentAuthor) {
			t.Errorf("delete with no author = %v, want ErrNotCommentAuthor", err)
		}
		if err := b.DeleteFindingComment(ctx, flowID, others, author); !errors.Is(err, interfaces.ErrNotCommentAuthor) {
			t.Errorf("delete of another author's comment = %v, want ErrNotCommentAuthor", err)
		}
		if n := count(); n != 2 {
			t.Fatalf("%d comments after refused deletes, want 2", n)
		}
		// A delete names the flow it was authorized for: another flow's id reaches nothing, for the author and the
		// moderator alike.
		elsewhere := "contract-flow-comments-elsewhere-" + runID
		if err := b.DeleteFindingComment(ctx, elsewhere, own, author); err != nil {
			t.Errorf("delete through another flow = %v, want nil (nothing there)", err)
		}
		if err := b.DeleteFindingCommentAsModerator(ctx, elsewhere, others); err != nil {
			t.Errorf("moderator delete through another flow = %v, want nil (nothing there)", err)
		}
		if n := count(); n != 2 {
			t.Fatalf("%d comments after deletes through another flow, want 2", n)
		}
		if err := b.DeleteFindingComment(ctx, flowID, own, author); err != nil {
			t.Errorf("delete of one's own comment: %v", err)
		}
		if err := b.DeleteFindingComment(ctx, flowID, own, author); err != nil {
			t.Errorf("delete of an absent comment = %v, want nil", err)
		}
		if err := b.DeleteFindingCommentAsModerator(ctx, flowID, others); err != nil {
			t.Errorf("moderator delete: %v", err)
		}
		if err := b.DeleteFindingCommentAsModerator(ctx, flowID, others); err != nil {
			t.Errorf("moderator delete of an absent comment = %v, want nil", err)
		}
		if n := count(); n != 0 {
			t.Errorf("%d comments left, want 0", n)
		}
	})
}
