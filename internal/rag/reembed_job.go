package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// reembedJob is the on-demand background re-embed of an org's knowledge base (ADR 0027): the admin's re-index
// with {"background": true} runs ReindexOrg page after page until the corpus is converted, outside the request,
// instead of the client resuming it request by request. One job for every org: its lease serialises the runs
// across replicas, one org at a time — an admin action, rare and long. It never starts by itself: a model switch
// is detected and reported (stranded counts, pad_rag_stranded_search_total), never repaired by unasked spend.
var reembedJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:     "knowledge_reembed",
	Schedule: "on demand (admin re-index in the background; server store, org-scoped embedding credential)",
})

// reembedRunTimeout bounds one background run: a corpus of maxKnowledgeChunks per document over the per-request
// budget of ReindexOrg, with retries, fits it with room; a run cut by it resumes from the admin's next start.
const reembedRunTimeout = 2 * time.Hour

// reembedRequest is what the next run converts: the org, under whose credential scope and RLS identity, and
// whether every chunk is rewritten (force) or only those not yet at the caller's model.
type reembedRequest struct {
	scope, orgID, userID string
	force                bool
}

// ReindexInBackground starts the knowledge_reembed job for orgID and returns its name; the run converts the
// corpus as ReindexOrg(force) would, resuming its own pages. It runs unattended under the credential it was
// started with, so it is refused unless that credential is the organisation's shared one
// (ErrEmbeddingOrgCredentialRequired): a member's personal key must not be spent after they left, and the org
// owns its corpus. ErrKnowledgeBusy while a run is in flight on any replica; ErrReembedUnavailable when the
// job's lock could not be asked for or the server is stopping.
func (s *KnowledgeService) ReindexInBackground(ctx context.Context, scope, orgID string, force bool) (string, error) {
	if s.store == nil {
		return "", fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	ctx = ai.WithUsageScope(ctx, scope, orgID) // the org's policy applies to the start as to every page
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return "", fmt.Errorf("embedding provider unavailable: %w", err)
	}
	if !s.onOrgCredential(provider) {
		return "", ErrEmbeddingOrgCredentialRequired
	}
	req := &reembedRequest{scope: scope, orgID: orgID, force: force}
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		req.userID = claims.UserID
	}

	s.reembedMu.Lock()
	defer s.reembedMu.Unlock()
	if s.reembedPending != nil {
		// Started a moment ago; its run has not taken its request yet.
		return "", ErrKnowledgeBusy
	}
	s.reembedPending = req
	err = s.reembedRunner().RunNow(ctx)
	switch {
	case err == nil:
		return reembedJob.Name(), nil
	case errors.Is(err, lifecycle.ErrJobHeld):
		s.reembedPending = nil
		return "", ErrKnowledgeBusy
	default:
		s.reembedPending = nil
		return "", fmt.Errorf("%w: %w", ErrReembedUnavailable, err)
	}
}

// onOrgCredential reports whether provider runs on the organisation's shared credential, the only one a
// background run may spend (ADR 0027). The embedder override (WithEmbedder) is a test hook, nil in production:
// the tests' stand-in for the org credential, never anyone's personal key.
func (s *KnowledgeService) onOrgCredential(provider ai.Provider) bool {
	return ai.UsedOrgCredential(provider) || s.embedOverride != nil
}

// reembedRunner builds the job's runner once: under the deployment's job lease when one was wired
// (WithJobLeaser), else the process runs it alone; the store keeps the run state either way. Called under reembedMu.
func (s *KnowledgeService) reembedRunner() *lifecycle.Singleton {
	if s.reembed == nil {
		s.reembed = lifecycle.NewSingleton(lifecycle.Job{Spec: reembedJob, Timeout: reembedRunTimeout, Run: s.runReembed}, s.jobLeaser, s.store)
	}
	return s.reembed
}

// StopJobs stops the background re-embed, cancelling a run in flight; shutdown does the same through
// lifecycle.DrainAll, tests call it at cleanup.
func (s *KnowledgeService) StopJobs() {
	s.reembedMu.Lock()
	job := s.reembed
	s.reembedMu.Unlock()
	if job != nil {
		job.Stop()
	}
}

// runReembed is the job's run: the pending request's org, ReindexOrg request after request from the start until
// no cursor comes back, as the admin who started it (RLS) and under their credential scope. A request's
// failure ends the run with the pages before it kept (ReindexOrg commits per page); the next start resumes
// from where the identity filter finds unconverted chunks.
func (s *KnowledgeService) runReembed(ctx context.Context) error {
	s.reembedMu.Lock()
	req := s.reembedPending
	s.reembedPending = nil
	s.reembedMu.Unlock()
	if req == nil {
		return errors.New("knowledge re-embed started without a request")
	}
	if req.userID != "" {
		ctx = auth.WithClaims(ctx, &auth.Claims{UserID: req.userID})
	}
	var cursor interfaces.KnowledgeChunkCursor
	total, requests := 0, 0
	for {
		// Every page re-checks the credential rule: the org's shared one, or nothing.
		res, err := s.reindexOrg(ctx, req.scope, req.orgID, req.force, cursor, true)
		total += res.Chunks
		requests++
		if err != nil {
			return fmt.Errorf("knowledge re-embed of org %s stopped after %d chunks: %w", req.orgID, total, err)
		}
		if res.NextCursor == nil {
			logger.Info("knowledge base re-embedded in the background", "org_id", req.orgID, "chunks", total, "requests", requests, "force", req.force)
			return nil
		}
		cursor = *res.NextCursor
	}
}
