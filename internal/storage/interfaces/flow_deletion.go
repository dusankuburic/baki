package interfaces

import (
	"context"
	"errors"
)

// ErrFlowDeletionNotAuthorized is DeleteFlow's refusal under a row-level-security user when the caller did not state
// that it authorised the deletion (WithFlowDeletionAuthorized). Deleting a flow deletes every user's conversations
// on it as the system (R-security-3): the store does that only on the word of a caller that checked FlowDelete. A
// programming error, not a user's: it is not mapped to a 4xx.
var ErrFlowDeletionNotAuthorized = errors.New("delete flow: the deletion was not authorised (interfaces.WithFlowDeletionAuthorized after the FlowDelete check)")

type flowDeletionKey struct{}

// WithFlowDeletionAuthorized is ctx carrying the caller's statement that it authorised the deletion of flowID
// (FlowDelete: the flow's owner or an admin of its organisation). Only the code that made that check sets it
// (LibraryService.DeleteLibraryFlow).
func WithFlowDeletionAuthorized(ctx context.Context, flowID string) context.Context {
	return context.WithValue(ctx, flowDeletionKey{}, flowID)
}

// FlowDeletionAuthorized reports whether ctx carries the authorisation to delete flowID.
func FlowDeletionAuthorized(ctx context.Context, flowID string) bool {
	id, ok := ctx.Value(flowDeletionKey{}).(string)
	return ok && flowID != "" && id == flowID
}
