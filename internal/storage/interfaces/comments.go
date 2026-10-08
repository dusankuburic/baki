package interfaces

import (
	"context"
	"errors"
	"time"
)

// ErrNotCommentAuthor is returned by DeleteFindingComment when the comment belongs to someone else or no author is given (map to HTTP 403).
var ErrNotCommentAuthor = errors.New("cannot delete another user's comment")

// CommentStore covers team-shared review threads on individual findings.
type CommentStore interface {
	AddFindingComment(ctx context.Context, c *FindingComment) error
	ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*FindingComment, error)
	// DeleteFindingComment removes authorID's own comment: someone else's returns ErrNotCommentAuthor, and so does an empty authorID, which matches no author; absent is a no-op.
	DeleteFindingComment(ctx context.Context, flowID, commentID, authorID string) error
	// DeleteFindingCommentAsModerator removes a comment whoever wrote it; the caller has checked the right to moderate (FlowShare); absent is a no-op.
	DeleteFindingCommentAsModerator(ctx context.Context, flowID, commentID string) error
}

// FindingComment is a team-shared review comment keyed by the finding's stable key (same as triage); comments persist across re-analysis.
type FindingComment struct {
	ID         string    `json:"id"`
	FlowID     string    `json:"flowId"`
	FindingKey string    `json:"findingKey"`
	AuthorID   string    `json:"authorId"`
	AuthorName string    `json:"authorName,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}
