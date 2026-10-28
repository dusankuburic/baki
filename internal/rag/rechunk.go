package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// rechunkListPage is how many documents below the current chunker version one listing of RechunkOrg reads.
const rechunkListPage = 100

// RechunkOrg is the rechunk re-index (R-knowledge-9): every ready document of the org cut by an older chunker version
// is cut again at the current one (chunkerVersion) from its kept source text and embedded with the caller's
// embedding provider, as a re-upload of the same name and text would be — the new version pending until all of it
// is embedded, then replacing the old one at once (UploadDocument), the upload's model guard included. A plain
// re-index (ReindexOrg) never re-chunks: it re-embeds the stored chunks as they are.
//
// A document stored before the source text was kept (v51) has nothing to cut again: its chunks are all there is, and
// they cannot be put back together — their order is not stored (chunk ids are random), a carried overlap is dropped
// where a paragraph did not fit beside it, the joints are blanks or blank lines, and version 1 already altered the
// text it split at sentence ends. Such a document is skipped and counted (SkippedNoSource): it stays at its version,
// searchable as before, until it is uploaded again. So is one whose name has an unfinished upload of other text
// (SkippedPending): that upload is the newer version, which a rechunk must neither delete nor overwrite with the
// older text — re-uploading its own text would supersede it. A pending version of the same text (a rechunk's own,
// left unfinished by an earlier request) is resumed.
//
// Within the request budget (reindexRequestBudget) the walk goes document by document; a document left unfinished
// is answered with a NextCursor and the next request resumes it (a pending version of the same name and text is
// continued, never stored again), so the walk resumes from the corpus itself: the cursor says more is left and is
// not needed to resume. The org's lease is held over the request, as an upload's is.
func (s *KnowledgeService) RechunkOrg(ctx context.Context, scope, orgID string) (ReindexResult, error) {
	if s.store == nil {
		return ReindexResult{}, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	ctx = usageRun(ctx, scope, orgID)
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	callerCtx := ctx
	ctx, unlock, err := s.lockOrg(ctx, orgID)
	if err != nil {
		return ReindexResult{}, err
	}
	defer unlock()
	defer s.bus.Publish(callerCtx, events.KnowledgeDocChanged{OrgID: orgID, Kind: events.KnowledgeDocReindexed})

	start := time.Now()
	var res ReindexResult
	worked := map[string]bool{}
	// stop answers what this request did so far; more is set when a document is left unfinished.
	stop := func(more *interfaces.KnowledgeChunkCursor, err error) (ReindexResult, error) {
		res.Docs = len(worked)
		if more != nil {
			res.NextCursor, res.Truncated = more, true
		}
		if err != nil {
			err = lostOr(ctx, err)
			logger.Warn("knowledge base rechunk stopped; resumable", "org_id", orgID, "chunks", res.Chunks, "rechunked", res.Rechunked, "error", err)
		} else if res.Chunks > 0 || res.Rechunked > 0 {
			logger.Info("knowledge base rechunked", "org_id", orgID, "chunks", res.Chunks, "documents", res.Rechunked,
				"skipped_no_source", res.SkippedNoSource, "skipped_too_large", res.SkippedTooLarge,
				"skipped_pending", res.SkippedPending, "more", more != nil)
		}
		return res, err
	}
	after := ""
	for {
		docs, err := s.store.ListKnowledgeDocumentsBelowChunkerVersion(ctx, orgID, chunkerVersion, after, rechunkListPage)
		if err != nil {
			return stop(nil, fmt.Errorf("list the documents to rechunk: %w", err))
		}
		if len(docs) == 0 {
			return stop(nil, nil)
		}
		for _, d := range docs {
			after = d.ID
			if !d.HasSource {
				res.SkippedNoSource++
				continue
			}
			if len(worked) > 0 && time.Since(start) >= reindexRequestBudget {
				return stop(&interfaces.KnowledgeChunkCursor{DocID: d.ID}, nil)
			}
			full, err := s.store.GetKnowledgeDocument(ctx, orgID, d.ID)
			if errors.Is(err, interfaces.ErrNotFound) {
				continue // deleted meanwhile
			}
			if err != nil {
				return stop(nil, fmt.Errorf("load the document to rechunk: %w", err))
			}
			chunks := chunkDocument(chunkerVersion, full.Content, full.Filename)
			if len(chunks) == 0 || len(chunks) > maxKnowledgeChunks {
				logger.Warn("knowledge base rechunk: the document's new split is outside what one upload may index; left as it is",
					"org_id", orgID, "doc_id", d.ID, "chunks", len(chunks), "limit", maxKnowledgeChunks)
				res.SkippedTooLarge++
				continue
			}
			up, err := s.ingestLocked(ctx, provider, orgID, full.Filename, full.Content, chunks, start, true)
			if errors.Is(err, errPendingUploadOfName) {
				// An admin's upload of the name is unfinished: it is the newer version, and a rechunk never supersedes
				// it (nor brings the older text back over it). Skipped and reported; the upload, once finished,
				// replaces this document anyway.
				res.SkippedPending++
				continue
			}
			res.Chunks += up.Embedded
			if up.ID != "" {
				worked[up.ID] = true
			}
			var more *interfaces.KnowledgeChunkCursor
			if up.ID != "" && up.State != interfaces.KnowledgeDocumentReady {
				more = &interfaces.KnowledgeChunkCursor{DocID: d.ID, ID: up.ID}
			}
			if err != nil || more != nil {
				return stop(more, err)
			}
			res.Rechunked++
		}
	}
}
