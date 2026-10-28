package rag

import (
	"context"

	"pad-analyzer/internal/audit"
)

// AuditRecorder records an audit entry (audit.Recorder, whose nil records nothing): the knowledge service records the
// corpus changes no request handler sees — the identity stamp of proven legacy documents (R-knowledge-3).
type AuditRecorder interface {
	Record(ctx context.Context, e audit.Entry)
}

// WithAuditRecorder records the service's own corpus changes in the audit trail (see AuditRecorder).
func WithAuditRecorder(r AuditRecorder) Option {
	return func(s *KnowledgeService) { s.audit = r }
}
