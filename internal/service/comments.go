package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// CommentService owns the team-shared review threads on findings and the lookups their notifications
// need: who to alert and which assignee to email. The finding's assignee comes from TriageService,
// which owns triage state. Authorization is the caller's.
type CommentService struct {
	store  storageif.CommentStore
	triage *TriageService
}

func NewCommentService(store storageif.CommentStore, triage *TriageService) *CommentService {
	return &CommentService{store: store, triage: triage}
}

// List returns a finding's comments.
func (s *CommentService) List(ctx context.Context, flowID, findingKey string) ([]*storageif.FindingComment, error) {
	return s.store.ListFindingComments(ctx, flowID, findingKey)
}

// Add stores a comment; the store assigns its ID.
func (s *CommentService) Add(ctx context.Context, c *storageif.FindingComment) error {
	return s.store.AddFindingComment(ctx, c)
}

// Delete removes a comment. A non-empty authorID restricts the delete to that author's comment
// (storageif.ErrNotCommentAuthor otherwise); "" deletes unconditionally.
func (s *CommentService) Delete(ctx context.Context, flowID, commentID, authorID string) error {
	return s.store.DeleteFindingComment(ctx, flowID, commentID, authorID)
}

// AuthorName resolves the commenter's name for alert and email copy (TriageService.DisplayName).
func (s *CommentService) AuthorName(ctx context.Context, authorID, fallback string) string {
	return s.triage.DisplayName(ctx, authorID, fallback)
}

// AlertTargets is who a new comment alerts: the finding's assignee and everyone who commented before
// (commentID, the new comment, excluded). The actor is not removed here. Lookups are best-effort.
func (s *CommentService) AlertTargets(ctx context.Context, flowID, findingKey, commentID string) map[string]bool {
	targets := map[string]bool{}
	if assignee := s.triage.Assignee(ctx, flowID, findingKey); assignee != "" {
		targets[assignee] = true
	}
	if prior, err := s.store.ListFindingComments(ctx, flowID, findingKey); err == nil {
		for _, c := range prior {
			if c.AuthorID != "" && c.ID != commentID {
				targets[c.AuthorID] = true
			}
		}
	}
	return targets
}

// AssigneeToNotify returns the finding's assignee to email about a comment, nil when there is none,
// the commenter is the assignee, or the assignee has no email address.
func (s *CommentService) AssigneeToNotify(ctx context.Context, flowID, findingKey, commenterID string) *storageif.User {
	assigneeID := s.triage.Assignee(ctx, flowID, findingKey)
	if assigneeID == "" || assigneeID == commenterID {
		return nil
	}
	return s.triage.Recipient(ctx, assigneeID)
}
